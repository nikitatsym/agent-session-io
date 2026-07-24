package cli

import (
	"encoding/json"
	"fmt"

	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/spf13/cobra"
)

const versionSchema = "sessionio.version/v1"

type versionRecord struct {
	Schema string `json:"schema"`
	buildinfo.Info
}

// NewRoot creates the sessionio command tree.
func NewRoot(info buildinfo.Info) *cobra.Command {
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
	root.AddCommand(newVersionCommand(info))
	return root
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

func shortCommit(value string) string {
	const shortLength = 12
	if len(value) <= shortLength {
		return value
	}
	return value[:shortLength]
}
