package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/nikitatsym/agent-session-io/internal/completion"
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
}

// NewRoot creates the sessionio command tree.
func NewRoot(info buildinfo.Info) *cobra.Command {
	return newRoot(info, rootOptions{
		completionEnvironment: completion.CurrentEnvironment,
		newUpdater: func() (updateService, error) {
			return updater.New()
		},
	})
}

func newRoot(info buildinfo.Info, options rootOptions) *cobra.Command {
	root := &cobra.Command{
		Use:           "sessionio",
		Short:         "Read and search coding-agent sessions",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.AddCommand(
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

func newCompletionInstallCommand(
	environment func() (completion.Environment, error),
) *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:       "install [bash|fish|powershell|zsh]",
		Short:     "Install completion into the current shell",
		Args:      cobra.MaximumNArgs(1),
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
		Args:  cobra.NoArgs,
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
		Args:  cobra.NoArgs,
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
