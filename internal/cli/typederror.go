package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

const errorSchema = "sessionio.error/v1"

const (
	exitCapability = 3
	exitIntegrity  = 5
)

type errorRecord struct {
	Schema      string         `json:"schema"`
	Kind        string         `json:"kind"`
	Message     string         `json:"message"`
	Remediation string         `json:"remediation"`
	Details     map[string]any `json:"details"`
}

// ErrorReported tells main that the command already wrote a machine record.
func ErrorReported(err error) bool {
	var reported *commandError
	if errors.As(err, &reported) {
		return reported.reported
	}
	return false
}

func exitCodeForKind(kind catalog.Kind) int {
	switch kind {
	case catalog.KindConfigInvalid:
		return exitInvalid
	case catalog.KindPostgresNotConfigured,
		catalog.KindPostgresUnreachable,
		catalog.KindPostgresVersionUnsupported,
		catalog.KindPostgresCapabilityMissing,
		catalog.KindCatalogNotInitialized,
		catalog.KindCatalogSchemaMismatch:
		return exitCapability
	default:
		return exitIntegrity
	}
}

func typedRecord(err error) (errorRecord, bool) {
	var configError *config.Error
	if errors.As(err, &configError) {
		return errorRecord{
			Schema:      errorSchema,
			Kind:        string(catalog.KindConfigInvalid),
			Message:     configError.Error(),
			Remediation: configError.Remediation,
			Details:     configError.Details(),
		}, true
	}
	var catalogError *catalog.Error
	if errors.As(err, &catalogError) {
		details := catalogError.Details
		if details == nil {
			details = map[string]any{}
		}
		return errorRecord{
			Schema:      errorSchema,
			Kind:        string(catalogError.Kind),
			Message:     catalogError.Message,
			Remediation: catalogError.Remediation,
			Details:     details,
		}, true
	}
	return errorRecord{}, false
}

// typedFailure maps a typed failure to the fixed exit contract and, in
// machine format, writes exactly one error record to stdout.
func typedFailure(writer io.Writer, format outputFormat, err error) error {
	record, typed := typedRecord(err)
	if !typed {
		return &commandError{code: exitIntegrity, err: err}
	}
	code := exitCodeForKind(catalog.Kind(record.Kind))
	if format != formatJSON {
		return &commandError{code: code, err: err}
	}
	if encodeErr := json.NewEncoder(writer).Encode(record); encodeErr != nil {
		return &commandError{
			code: exitIntegrity,
			err:  fmt.Errorf("write error record: %w", errors.Join(encodeErr, err)),
		}
	}
	return &commandError{code: code, err: err, reported: true}
}
