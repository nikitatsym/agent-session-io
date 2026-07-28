package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/adapters/claude"
	"github.com/nikitatsym/agent-session-io/adapters/codex"
	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/nikitatsym/agent-session-io/internal/completion"
	"github.com/nikitatsym/agent-session-io/internal/config"
	runtimepresence "github.com/nikitatsym/agent-session-io/internal/presence"
	"github.com/nikitatsym/agent-session-io/internal/readercache"
	"github.com/nikitatsym/agent-session-io/internal/updater"
	"github.com/spf13/cobra"
)

const versionSchema = "sessionio.version/v1"

type versionRecord struct {
	Schema string `json:"schema"`
	buildinfo.Info
}

type updateService interface {
	Update(context.Context, string) (updater.Result, error)
}

type rootOptions struct {
	completionEnvironment func() (completion.Environment, error)
	newUpdater            func() (updateService, error)
	newRegistry           func(config.Sources, *readercache.Store) (*sessionio.Registry, error)
	newPresenceProviders  presenceProviderFactory
	now                   func() time.Time
}

// NewRoot creates the sessionio command tree.
func NewRoot(info buildinfo.Info) *cobra.Command {
	return newRoot(info, rootOptions{
		completionEnvironment: completion.CurrentEnvironment,
		newUpdater: func() (updateService, error) {
			return updater.New()
		},
		newRegistry:          newDefaultRegistry,
		newPresenceProviders: newDefaultPresenceProviders,
		now:                  time.Now,
	})
}

func newRoot(info buildinfo.Info, options rootOptions) *cobra.Command {
	if options.now == nil {
		options.now = time.Now
	}
	root := &cobra.Command{
		Use:           "sessionio",
		Short:         "Discover and read coding-agent sessions",
		Args:          invalidArgs(cobra.NoArgs),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return invalidUsage(err)
	})
	// Reader commands read this flag for source roots only; a PostgreSQL
	// connection stays exclusive to catalog-backed commands.
	var configPath string
	root.PersistentFlags().StringVar(
		&configPath,
		"config",
		"",
		"path to the sessionio configuration file",
	)
	// Every command that reads sessions resolves its roots the same way, so a
	// scan and a reader command always see the same corpus.
	var caches []*readercache.Store
	newRegistry := func() (*sessionio.Registry, *readercache.Store, error) {
		settings, err := loadReaderSettings(configPath)
		if err != nil {
			return nil, nil, err
		}
		store := readercache.Open(settings.cache)
		caches = append(caches, store)
		registry, err := options.newRegistry(settings.sources, store)
		if err != nil {
			return nil, nil, err
		}
		return registry, store, nil
	}
	// The listing cache is written after the command succeeded, so a failed
	// run never retains what it could not finish listing.
	root.PersistentPostRunE = func(cmd *cobra.Command, _ []string) error {
		for _, store := range caches {
			store.Flush()
			if err := writeCacheDiagnostics(cmd.ErrOrStderr(), store); err != nil {
				return err
			}
		}
		return nil
	}
	root.AddCommand(
		newSourcesCommand(info, newRegistry),
		newListCommand(
			info,
			newRegistry,
			options.newPresenceProviders,
			options.now,
		),
		newShowCommand(newRegistry),
		newExportCommand(info, newRegistry),
		newCatalogCommand(&configPath),
		newScanCommand(&configPath, newRegistry),
		newSearchCommand(&configPath),
		newDoctorCommand(&configPath),
		newUpdateCommand(info, options.newUpdater),
		newVersionCommand(info),
	)
	root.InitDefaultCompletionCmd()
	completionCommand := mustDirectChild(root, "completion")
	completionCommand.AddCommand(newCompletionInstallCommand(
		options.completionEnvironment,
	))
	return root
}

// newDefaultRegistry leaves an undeclared root empty, so the adapter keeps
// resolving the harness environment variable and then the platform default.
func newDefaultRegistry(
	sources config.Sources,
	cache *readercache.Store,
) (*sessionio.Registry, error) {
	codexConfig := codex.DefaultConfig()
	codexConfig.Home = sources.CodexHome()
	codexConfig.Cache = cache
	codexAdapter, err := codex.New(codexConfig)
	if err != nil {
		return nil, fmt.Errorf("configure Codex adapter: %w", err)
	}
	claudeConfig := claude.DefaultConfig()
	claudeConfig.ConfigDir = sources.ClaudeConfigDir()
	claudeConfig.Cache = cache
	claudeAdapter, err := claude.New(claudeConfig)
	if err != nil {
		return nil, fmt.Errorf("configure Claude adapter: %w", err)
	}
	return sessionio.NewRegistry(codexAdapter, claudeAdapter)
}

// writeCacheDiagnostics reports what the advisory cache could not do. It is a
// human line on stderr in every format: machine stdout never changes because a
// cache file was unusable.
func writeCacheDiagnostics(writer io.Writer, store *readercache.Store) error {
	for _, diagnostic := range store.Diagnostics() {
		if _, err := fmt.Fprintln(writer, diagnostic.String()); err != nil {
			return fmt.Errorf("write reader cache diagnostic: %w", err)
		}
	}
	return nil
}

func newDefaultPresenceProviders(
	harnesses []sessionio.Harness,
) ([]runtimepresence.Provider, error) {
	providers := make([]runtimepresence.Provider, 0, len(harnesses))
	for _, harness := range harnesses {
		var (
			provider runtimepresence.Provider
			err      error
		)
		switch harness {
		case sessionio.HarnessCodex:
			provider, err = runtimepresence.NewCodexOpenFileProvider(
				runtimepresence.CodexProviderConfig{},
			)
		case sessionio.HarnessClaude:
			provider, err = runtimepresence.NewClaudeProvider(
				runtimepresence.ClaudeProviderConfig{},
			)
		default:
			return nil, fmt.Errorf(
				"configure runtime presence: harness %q is unsupported",
				harness,
			)
		}
		if err != nil {
			return nil, fmt.Errorf(
				"configure %s runtime presence: %w",
				harness,
				err,
			)
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

func newCompletionInstallCommand(
	environment func() (completion.Environment, error),
) *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "install [bash|fish|powershell|zsh]",
		Short: "Install completion into the current shell",
		Args: invalidArgs(cobra.MatchAll(
			cobra.MaximumNArgs(1),
			cobra.OnlyValidArgs,
		)),
		ValidArgs: []string{"bash", "fish", "powershell", "zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			currentEnvironment, err := environment()
			if err != nil {
				return err
			}
			requestedShell := ""
			if len(args) == 1 {
				requestedShell = args[0]
			}
			result, err := completion.Install(
				cmd.Root(),
				currentEnvironment,
				requestedShell,
				profile,
			)
			if err != nil {
				return fmt.Errorf("install shell completion: %w", err)
			}
			if _, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"installed %s completion to %s\n",
				result.Shell,
				result.ScriptPath,
			); err != nil {
				return fmt.Errorf("write completion result: %w", err)
			}
			for _, profilePath := range result.ProfilePaths {
				if _, err := fmt.Fprintf(
					cmd.OutOrStdout(),
					"connected completion in %s\n",
					profilePath,
				); err != nil {
					return fmt.Errorf("write profile result: %w", err)
				}
			}
			if _, err := fmt.Fprintln(
				cmd.OutOrStdout(),
				"restart the shell to activate completion",
			); err != nil {
				return fmt.Errorf("write activation hint: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(
		&profile,
		"profile",
		"",
		"PowerShell profile path",
	)
	return cmd
}

func newUpdateCommand(
	info buildinfo.Info,
	newUpdater func() (updateService, error),
) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update sessionio to the latest release",
		Args:  invalidArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := newUpdater()
			if err != nil {
				return fmt.Errorf("configure updater: %w", err)
			}
			result, err := service.Update(cmd.Context(), info.Version)
			if err != nil {
				return fmt.Errorf("update sessionio: %w", err)
			}
			if result.Updated {
				if _, err := fmt.Fprintf(
					cmd.OutOrStdout(),
					"updated sessionio from %s to %s\n",
					result.PreviousVersion,
					result.CurrentVersion,
				); err != nil {
					return fmt.Errorf("write update result: %w", err)
				}
				return nil
			}
			if _, err := fmt.Fprintf(
				cmd.OutOrStdout(),
				"sessionio %s is already up to date\n",
				result.CurrentVersion,
			); err != nil {
				return fmt.Errorf("write update result: %w", err)
			}
			return nil
		},
	}
}

func newVersionCommand(info buildinfo.Info) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  invalidArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				if err := encoder.Encode(versionRecord{
					Schema: versionSchema,
					Info:   info,
				}); err != nil {
					return fmt.Errorf("encode version JSON: %w", err)
				}
				return nil
			}

			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "sessionio %s", info.Version); err != nil {
				return fmt.Errorf("write version: %w", err)
			}
			if info.Commit != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), " (%s)", shortCommit(info.Commit)); err != nil {
					return fmt.Errorf("write commit: %w", err)
				}
			}
			if info.Dirty {
				if _, err := fmt.Fprint(cmd.OutOrStdout(), " dirty"); err != nil {
					return fmt.Errorf("write dirty state: %w", err)
				}
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("finish version output: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "write version information as JSON")
	return cmd
}

func mustDirectChild(root *cobra.Command, name string) *cobra.Command {
	for _, command := range root.Commands() {
		if command.Name() == name {
			return command
		}
	}
	panic(fmt.Sprintf("cobra did not initialize the %s command", name))
}

func shortCommit(value string) string {
	const shortLength = 12
	if len(value) <= shortLength {
		return value
	}
	return value[:shortLength]
}
