package catalog

import (
	"context"
	"errors"
	"fmt"
)

const (
	StatusOK    = "ok"
	StatusError = "error"
)

type Check struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	Checks []Check
}

// Status reports overall health; any failed check fails the report.
func (report Report) Status() string {
	for _, check := range report.Checks {
		if check.Status != StatusOK {
			return StatusError
		}
	}
	return StatusOK
}

func (report *Report) ok(name, detail string) {
	report.Checks = append(report.Checks, Check{
		Name:   name,
		Status: StatusOK,
		Detail: detail,
	})
}

func (report *Report) fail(name, detail, remediation string) {
	report.Checks = append(report.Checks, Check{
		Name:        name,
		Status:      StatusError,
		Detail:      detail,
		Remediation: remediation,
	})
}

// FailedReport turns a typed connection or configuration failure into the
// report shape, so doctor never crashes on an absent PostgreSQL. An untyped
// failure is returned unchanged.
func FailedReport(err error) (Report, error) {
	var report Report
	if !report.failTyped("connection", err) {
		return Report{}, err
	}
	return report, nil
}

// failTyped records a typed failure as a check and reports whether it was one.
func (report *Report) failTyped(name string, err error) bool {
	var typed *Error
	if !errors.As(err, &typed) {
		return false
	}
	report.fail(
		name,
		fmt.Sprintf("%s: %s", typed.Kind, typed.Message),
		typed.Remediation,
	)
	return true
}

// CatalogStatus is the doctor-level probe of the configured schema.
type CatalogStatus struct {
	SchemaExists bool
	Revision     int
}

// Status probes the catalog schema without creating anything.
func (catalog *Catalog) Status(ctx context.Context) (CatalogStatus, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return CatalogStatus{}, err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return CatalogStatus{}, fmt.Errorf(
			"acquire PostgreSQL connection: %w",
			err,
		)
	}
	defer connection.Release()
	return catalog.status(ctx, connection)
}

func (catalog *Catalog) status(
	ctx context.Context,
	connection queryRunner,
) (CatalogStatus, error) {
	var exists bool
	if err := connection.QueryRow(
		ctx,
		"SELECT to_regnamespace($1) IS NOT NULL",
		catalog.settings.SchemaName,
	).Scan(&exists); err != nil {
		return CatalogStatus{}, fmt.Errorf("look up catalog schema: %w", err)
	}
	if !exists {
		return CatalogStatus{}, catalog.notInitialized("schema is absent")
	}
	present, err := catalog.tableExists(ctx, connection, "catalog_meta")
	if err != nil {
		return CatalogStatus{}, err
	}
	if !present {
		return CatalogStatus{SchemaExists: true}, catalog.notInitialized(
			"catalog_meta is absent",
		)
	}
	var revision int
	if err := connection.QueryRow(ctx, fmt.Sprintf(
		"SELECT revision FROM %s.catalog_meta",
		catalog.schema,
	)).Scan(&revision); err != nil {
		return CatalogStatus{}, fmt.Errorf("read catalog revision: %w", err)
	}
	if revision != Revision {
		return CatalogStatus{SchemaExists: true, Revision: revision},
			revisionMismatch(catalog.settings.SchemaName, revision)
	}
	return CatalogStatus{SchemaExists: true, Revision: revision}, nil
}

func (catalog *Catalog) notInitialized(detail string) error {
	return &Error{
		Kind: KindCatalogNotInitialized,
		Message: fmt.Sprintf(
			"catalog schema %s is not initialized: %s",
			catalog.settings.SchemaName,
			detail,
		),
		Remediation: "sessionio catalog init",
		Details: map[string]any{
			"catalog_schema": catalog.settings.SchemaName,
		},
	}
}

// Doctor runs the PostgreSQL diagnostic checks in a fixed order.
func (catalog *Catalog) Doctor(ctx context.Context) (Report, error) {
	var report Report
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return FailedReport(err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("acquire PostgreSQL connection: %w", err)
	}
	defer connection.Release()
	facts, err := catalog.probe(ctx, connection)
	if err != nil {
		return Report{}, err
	}
	report.ok("connection", fmt.Sprintf(
		"current_user=%s database=%s",
		facts.User,
		facts.Database,
	))
	major, versionErr := checkPostgresMajor(facts.ServerVersionNum)
	if versionErr != nil {
		return versionFailureReport(report, versionErr)
	}
	report.ok("server_version", fmt.Sprintf(
		"major=%d server_version_num=%d",
		major,
		facts.ServerVersionNum,
	))
	catalog.reportCapabilities(&report, facts)
	if err := catalog.reportCatalog(ctx, &report, connection, facts); err != nil {
		return Report{}, err
	}
	return report, nil
}

// versionFailureReport stops the run: no schema check survives a wrong major.
func versionFailureReport(report Report, err error) (Report, error) {
	if !report.failTyped("server_version", err) {
		return Report{}, err
	}
	return report, nil
}

func (catalog *Catalog) reportCapabilities(report *Report, facts probeFacts) {
	problems := map[string]capabilityProblem{}
	for _, problem := range capabilityProblems(facts) {
		problems[problem.Item] = problem
	}
	if problem, failed := problems["shared_preload_libraries"]; failed {
		report.fail(
			"preload",
			problem.Detail,
			"preload pg_textsearch through shared_preload_libraries and"+
				" restart PostgreSQL",
		)
	} else {
		report.ok("preload", "pg_textsearch is preloaded")
	}
	for _, requirement := range requiredExtensions {
		name := "extension_" + requirement.name
		if problem, failed := problems[name]; failed {
			report.fail(
				name,
				problem.Detail,
				"install the pinned extension set from the canonical"+
					" PostgreSQL profile in postgres/compose.yaml",
			)
			continue
		}
		report.ok(name, fmt.Sprintf(
			"%s version %s is installed",
			requirement.name,
			facts.Extensions[requirement.name].Installed,
		))
	}
}

// reportCatalog records catalog checks; an untyped failure is a runtime error.
func (catalog *Catalog) reportCatalog(
	ctx context.Context,
	report *Report,
	connection queryRunner,
	facts probeFacts,
) error {
	if facts.SchemaExists {
		report.ok("schema_exists", fmt.Sprintf(
			"schema %s exists",
			catalog.settings.SchemaName,
		))
	} else {
		report.fail(
			"schema_exists",
			fmt.Sprintf(
				"%s: schema %s does not exist",
				KindCatalogNotInitialized,
				catalog.settings.SchemaName,
			),
			"sessionio catalog init",
		)
	}
	status, err := catalog.status(ctx, connection)
	if err := report.record("catalog_revision", fmt.Sprintf(
		"revision %d matches this build",
		status.Revision,
	), err); err != nil {
		return err
	}
	catalog.reportPrivileges(report, facts)
	generation, err := catalog.activeGeneration(ctx, connection, facts)
	return report.record("active_generation", generation, err)
}

// record appends an ok check for a nil error and a failed check for a typed
// one; an untyped failure is returned as a runtime error.
func (report *Report) record(name string, detail string, err error) error {
	if err == nil {
		report.ok(name, detail)
		return nil
	}
	if report.failTyped(name, err) {
		return nil
	}
	return err
}

func (catalog *Catalog) reportPrivileges(report *Report, facts probeFacts) {
	switch {
	case facts.SchemaExists && facts.SchemaUsable:
		report.ok("privileges", fmt.Sprintf(
			"role %s holds USAGE and CREATE on schema %s",
			facts.User,
			catalog.settings.SchemaName,
		))
	case !facts.SchemaExists && facts.CanCreateDatabase:
		report.ok("privileges", fmt.Sprintf(
			"role %s may create schema %s",
			facts.User,
			catalog.settings.SchemaName,
		))
	default:
		report.fail(
			"privileges",
			fmt.Sprintf(
				"role %s may neither create schema %s nor use it",
				facts.User,
				catalog.settings.SchemaName,
			),
			"grant the role CREATE on the database or USAGE and CREATE on"+
				" the configured schema",
		)
	}
}

// activeGeneration describes the pointer for doctor; none is valid.
func (catalog *Catalog) activeGeneration(
	ctx context.Context,
	connection queryRunner,
	facts probeFacts,
) (string, error) {
	if !facts.SchemaExists {
		return "none", nil
	}
	present, err := catalog.tableExists(ctx, connection, "active_generation")
	if err != nil {
		return "", err
	}
	if !present {
		return "none", nil
	}
	var generation *int64
	if err := connection.QueryRow(ctx, fmt.Sprintf(
		"SELECT max(generation_id) FROM %s.active_generation",
		catalog.schema,
	)).Scan(&generation); err != nil {
		return "", fmt.Errorf("read the active generation pointer: %w", err)
	}
	if generation == nil {
		return "none", nil
	}
	return fmt.Sprintf("generation %d", *generation), nil
}
