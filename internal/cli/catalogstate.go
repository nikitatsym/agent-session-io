package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/spf13/cobra"
)

const (
	stateExportSchema = "sessionio.catalog.state-export/v1"
	stateImportSchema = "sessionio.catalog.state-import/v1"
)

type stateRecord struct {
	Schema        string              `json:"schema"`
	CatalogSchema string              `json:"catalog_schema"`
	Path          string              `json:"path"`
	Counts        catalog.StateCounts `json:"counts"`
	Checksum      string              `json:"checksum"`
}

func newCatalogStateCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Export and import retained catalog state",
		Args:  invalidArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newCatalogStateExportCommand(configPath),
		newCatalogStateImportCommand(configPath),
	)
	return cmd
}

func newCatalogStateExportCommand(configPath *string) *cobra.Command {
	var formatValue string
	var output string
	cmd := &cobra.Command{
		Use:               "export",
		Short:             "Write retained evidence and checkpoints to a stream",
		Args:              invalidArgs(cobra.NoArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCatalog(
				cmd,
				*configPath,
				formatValue,
				"catalog state export",
				func(format outputFormat, opened *catalog.Catalog) error {
					summary, err := exportState(cmd, opened, output)
					if err != nil {
						return typedFailure(cmd.OutOrStdout(), format, err)
					}
					return writeStateRecord(cmd, format, stateRecord{
						Schema:        stateExportSchema,
						CatalogSchema: opened.SchemaName(),
						Path:          output,
						Counts:        summary.Counts,
						Checksum:      summary.Checksum,
					})
				},
			)
		},
	}
	addStateFlags(cmd, &formatValue, &output, "output", "path of the state stream to write")
	return cmd
}

func newCatalogStateImportCommand(configPath *string) *cobra.Command {
	var formatValue string
	var input string
	cmd := &cobra.Command{
		Use:               "import",
		Short:             "Load retained evidence and checkpoints from a stream",
		Args:              invalidArgs(cobra.NoArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCatalog(
				cmd,
				*configPath,
				formatValue,
				"catalog state import",
				func(format outputFormat, opened *catalog.Catalog) error {
					summary, err := importState(cmd, opened, input)
					if err != nil {
						return typedFailure(cmd.OutOrStdout(), format, err)
					}
					return writeStateRecord(cmd, format, stateRecord{
						Schema:        stateImportSchema,
						CatalogSchema: opened.SchemaName(),
						Path:          input,
						Counts:        summary.Counts,
						Checksum:      summary.Checksum,
					})
				},
			)
		},
	}
	addStateFlags(cmd, &formatValue, &input, "input", "path of the state stream to read")
	return cmd
}

func addStateFlags(
	cmd *cobra.Command,
	formatValue *string,
	path *string,
	name string,
	usage string,
) {
	addFormatFlag(
		cmd,
		formatValue,
		string(formatHuman),
		"output format: human or json",
		"human",
		"json",
	)
	cmd.Flags().StringVar(path, name, "", usage)
}

// exportState refuses to overwrite an existing stream: a state export is the
// only copy of retained evidence before a pre-freeze reset.
func exportState(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	output string,
) (summary catalog.StateSummary, err error) {
	if output == "" {
		return catalog.StateSummary{}, invalidUsage(
			errors.New("catalog state export requires --output"),
		)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return catalog.StateSummary{}, invalidUsage(fmt.Errorf(
			"catalog state export refuses to overwrite %s",
			output,
		))
	}
	if err != nil {
		return catalog.StateSummary{}, fmt.Errorf(
			"create the state stream %s: %w",
			output,
			err,
		)
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	return opened.ExportState(cmd.Context(), file, time.Now().UTC())
}

func importState(
	cmd *cobra.Command,
	opened *catalog.Catalog,
	input string,
) (summary catalog.StateSummary, err error) {
	if input == "" {
		return catalog.StateSummary{}, invalidUsage(
			errors.New("catalog state import requires --input"),
		)
	}
	file, err := os.Open(input)
	if err != nil {
		return catalog.StateSummary{}, fmt.Errorf(
			"open the state stream %s: %w",
			input,
			err,
		)
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	return opened.ImportState(cmd.Context(), file)
}

func writeStateRecord(
	cmd *cobra.Command,
	format outputFormat,
	record stateRecord,
) error {
	if format == formatJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(record); err != nil {
			return fmt.Errorf("write catalog state record: %w", err)
		}
		return nil
	}
	verb := "exported"
	if record.Schema == stateImportSchema {
		verb = "imported"
	}
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"catalog schema %q %s %s\n"+
			"sources: %d\n"+
			"source occurrences: %d\n"+
			"snapshot blobs: %d\n"+
			"session revisions: %d\n"+
			"scan checkpoints: %d\n"+
			"checksum: %s\n",
		record.CatalogSchema,
		verb,
		record.Path,
		record.Counts.Sources,
		record.Counts.Occurrences,
		record.Counts.SnapshotBlobs,
		record.Counts.SessionRevisions,
		record.Counts.Checkpoints,
		record.Checksum,
	); err != nil {
		return fmt.Errorf("write catalog state result: %w", err)
	}
	return nil
}
