package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/config"
	"github.com/spf13/cobra"
)

const doctorSchema = "sessionio.doctor/v1"

const doctorScopePostgres = "postgres"

type doctorRecord struct {
	Schema string          `json:"schema"`
	Scope  string          `json:"scope"`
	Status string          `json:"status"`
	Checks []catalog.Check `json:"checks"`
}

func newDoctorCommand(configPath *string) *cobra.Command {
	var formatValue string
	var scopeValue string
	cmd := &cobra.Command{
		Use:               "doctor",
		Short:             "Diagnose the configured PostgreSQL catalog",
		Args:              invalidArgs(cobra.NoArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := parseCatalogFormat(formatValue, "doctor")
			if err != nil {
				return err
			}
			scope, err := parseDoctorScope(scopeValue)
			if err != nil {
				return err
			}
			report, err := runDoctor(cmd, *configPath)
			if err != nil {
				return typedFailure(cmd.OutOrStdout(), format, err)
			}
			return writeDoctorReport(cmd, format, scope, report)
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
	cmd.Flags().StringVar(
		&scopeValue,
		"scope",
		doctorScopePostgres,
		"diagnostic scope: postgres",
	)
	registerFixedFlagCompletion(cmd, "scope", doctorScopePostgres)
	return cmd
}

func parseDoctorScope(value string) (string, error) {
	if value == doctorScopePostgres {
		return value, nil
	}
	return "", invalidUsage(fmt.Errorf(
		"invalid scope %q (expected %s)",
		value,
		doctorScopePostgres,
	))
}

func runDoctor(cmd *cobra.Command, configPath string) (catalog.Report, error) {
	opened, err := openCatalog(configPath)
	if err != nil {
		return doctorFailureReport(err)
	}
	defer opened.Close()
	return opened.Doctor(cmd.Context())
}

// doctorFailureReport turns an unreachable or unconfigured endpoint into a
// failed check report; only invalid configuration stays a typed failure.
func doctorFailureReport(err error) (catalog.Report, error) {
	if configInvalid(err) {
		return catalog.Report{}, err
	}
	return catalog.FailedReport(err)
}

func configInvalid(err error) bool {
	var configError *config.Error
	if errors.As(err, &configError) {
		return true
	}
	var catalogError *catalog.Error
	if errors.As(err, &catalogError) {
		return catalogError.Kind == catalog.KindConfigInvalid
	}
	return false
}

func writeDoctorReport(
	cmd *cobra.Command,
	format outputFormat,
	scope string,
	report catalog.Report,
) error {
	status := report.Status()
	if format == formatJSON {
		checks := report.Checks
		if checks == nil {
			checks = []catalog.Check{}
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(doctorRecord{
			Schema: doctorSchema,
			Scope:  scope,
			Status: status,
			Checks: checks,
		}); err != nil {
			return fmt.Errorf("write doctor record: %w", err)
		}
	} else if err := writeDoctorHuman(cmd, scope, report); err != nil {
		return err
	}
	if status == catalog.StatusOK {
		return nil
	}
	return &commandError{
		code: exitCapability,
		err: fmt.Errorf(
			"doctor scope %s reported %d failing checks",
			scope,
			failingChecks(report),
		),
		reported: true,
	}
}

func writeDoctorHuman(
	cmd *cobra.Command,
	scope string,
	report catalog.Report,
) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"%s: %s %s\n",
			check.Name,
			check.Status,
			check.Detail,
		); err != nil {
			return fmt.Errorf("write doctor check: %w", err)
		}
	}
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"doctor %s: %s\n",
		scope,
		report.Status(),
	); err != nil {
		return fmt.Errorf("write doctor status: %w", err)
	}
	for _, check := range report.Checks {
		if check.Status == catalog.StatusOK || check.Remediation == "" {
			continue
		}
		if _, err := fmt.Fprintf(
			cmd.ErrOrStderr(),
			"%s: remediation: %s\n",
			check.Name,
			check.Remediation,
		); err != nil {
			return fmt.Errorf("write doctor remediation: %w", err)
		}
	}
	return nil
}

func failingChecks(report catalog.Report) int {
	failing := 0
	for _, check := range report.Checks {
		if check.Status != catalog.StatusOK {
			failing++
		}
	}
	return failing
}
