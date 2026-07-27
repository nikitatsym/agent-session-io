package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/config"
	"github.com/spf13/cobra"
)

const catalogInitSchema = "sessionio.catalog.init/v1"

type catalogInitRecord struct {
	Schema          string `json:"schema"`
	PostgresMajor   int    `json:"postgres_major"`
	CatalogSchema   string `json:"catalog_schema"`
	CatalogRevision int    `json:"catalog_revision"`
	Created         bool   `json:"created"`
}

func newCatalogCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Manage the PostgreSQL catalog",
		Args:  invalidArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newCatalogInitCommand(configPath),
		newCatalogStateCommand(configPath),
	)
	return cmd
}

func newCatalogInitCommand(configPath *string) *cobra.Command {
	var formatValue string
	cmd := &cobra.Command{
		Use:               "init",
		Short:             "Create or verify the configured catalog schema",
		Args:              invalidArgs(cobra.NoArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCatalog(
				cmd,
				*configPath,
				formatValue,
				"catalog init",
				func(format outputFormat, opened *catalog.Catalog) error {
					result, err := opened.Init(cmd.Context())
					if err != nil {
						return typedFailure(cmd.OutOrStdout(), format, err)
					}
					return writeCatalogInit(cmd, format, result)
				},
			)
		},
	}
	addFormatFlag(
		cmd,
		&formatValue,
		string(formatHuman),
		"output format: human or json",
		"human",
		"json",
	)
	return cmd
}

func writeCatalogInit(
	cmd *cobra.Command,
	format outputFormat,
	result catalog.InitResult,
) error {
	if format == formatJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(catalogInitRecord{
			Schema:          catalogInitSchema,
			PostgresMajor:   result.PostgresMajor,
			CatalogSchema:   result.CatalogSchema,
			CatalogRevision: result.CatalogRevision,
			Created:         result.Created,
		}); err != nil {
			return fmt.Errorf("write catalog init record: %w", err)
		}
		return nil
	}
	state := "was already initialized at"
	if result.Created {
		state = "was initialized at"
	}
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"catalog schema %q %s revision %d on PostgreSQL %d\n",
		result.CatalogSchema,
		state,
		result.CatalogRevision,
		result.PostgresMajor,
	); err != nil {
		return fmt.Errorf("write catalog init result: %w", err)
	}
	return nil
}

// withCatalog rejects an invalid format before any connection is attempted, so
// a usage error never depends on PostgreSQL being reachable.
func withCatalog(
	cmd *cobra.Command,
	configPath string,
	formatValue string,
	name string,
	run func(outputFormat, *catalog.Catalog) error,
) error {
	format, err := parseCatalogFormat(formatValue, name)
	if err != nil {
		return err
	}
	opened, err := openCatalog(configPath)
	if err != nil {
		return typedFailure(cmd.OutOrStdout(), format, err)
	}
	defer opened.Close()
	return run(format, opened)
}

// parseCatalogFormat rejects ndjson: catalog commands emit one record.
func parseCatalogFormat(value string, command string) (outputFormat, error) {
	if outputFormat(value) == formatNDJSON {
		return "", invalidUsage(fmt.Errorf(
			"%s cannot stream, so ndjson is not an available format",
			command,
		))
	}
	return parseOutputFormat(value, formatHuman, formatJSON)
}

func openCatalog(configPath string) (*catalog.Catalog, error) {
	search, err := loadSearchConfig(configPath)
	if err != nil {
		return nil, err
	}
	settings, err := catalog.SettingsFromConfig(search)
	if err != nil {
		return nil, err
	}
	return catalog.New(settings)
}

func configFilePresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the default configuration path: %w", err)
	}
	return true, nil
}

// loadSources reads the declared source roots. Reader commands run without any
// configuration file, so an absent default path leaves discovery unchanged.
func loadSources(configPath string) (config.Sources, error) {
	path := configPath
	if path == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			return config.Sources{}, err
		}
		present, err := configFilePresent(defaultPath)
		if err != nil {
			return config.Sources{}, err
		}
		if !present {
			return config.Sources{}, nil
		}
		path = defaultPath
	}
	loaded, err := config.Load(path)
	if err != nil {
		return config.Sources{}, err
	}
	return loaded.Sources, nil
}

func loadSearchConfig(configPath string) (config.Search, error) {
	path := configPath
	if path == "" {
		defaultPath, err := config.DefaultPath()
		if err != nil {
			return config.Search{}, err
		}
		if _, err := os.Stat(defaultPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return config.Search{}, &catalog.Error{
					Kind: catalog.KindPostgresNotConfigured,
					Message: fmt.Sprintf(
						"no configuration file at %s",
						defaultPath,
					),
					Remediation: fmt.Sprintf(
						"create %s or pass --config with a configuration path",
						defaultPath,
					),
					Details: map[string]any{"default_path": defaultPath},
					Cause:   err,
				}
			}
			return config.Search{}, fmt.Errorf(
				"read the default configuration path: %w",
				err,
			)
		}
		path = defaultPath
	}
	loaded, err := config.Load(path)
	if err != nil {
		return config.Search{}, err
	}
	return loaded.Search, nil
}
