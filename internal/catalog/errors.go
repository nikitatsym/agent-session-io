package catalog

import "fmt"

// Kind is the stable machine identity of a catalog failure.
type Kind string

const (
	KindConfigInvalid               Kind = "config_invalid"
	KindPostgresNotConfigured       Kind = "postgres_not_configured"
	KindPostgresUnreachable         Kind = "postgres_unreachable"
	KindPostgresVersionUnsupported  Kind = "postgres_version_unsupported"
	KindPostgresCapabilityMissing   Kind = "postgres_capability_missing"
	KindCatalogNotInitialized       Kind = "catalog_not_initialized"
	KindCatalogSchemaMismatch       Kind = "catalog_schema_mismatch"
	KindCatalogGenerationIncomplete Kind = "catalog_generation_incomplete"
	KindCatalogStateCorrupt         Kind = "catalog_state_corrupt"
	KindCatalogStateTargetNotEmpty  Kind = "catalog_state_target_not_empty"
	KindScanInProgress              Kind = "scan_in_progress"
	KindSearchRequestInvalid        Kind = "search_request_invalid"
)

// Error is the typed catalog failure. Details never carry secrets.
type Error struct {
	Kind        Kind
	Message     string
	Remediation string
	Details     map[string]any
	// Cause is never rendered: connection causes quote the DSN.
	Cause error
}

func (catalogError *Error) Error() string {
	return fmt.Sprintf("%s: %s", catalogError.Kind, catalogError.Message)
}

func (catalogError *Error) Unwrap() error {
	return catalogError.Cause
}
