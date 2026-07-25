// Package catalog owns the PostgreSQL search catalog and its typed failures.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

const (
	maxConnections   = 4
	connectTimeout   = 10 * time.Second
	statementTimeout = "30s"
	lockTimeout      = "30s"
	// cleanupLockTimeout bounds the wait for readers of a dropped generation.
	cleanupLockTimeout = "3s"
)

const (
	sqlStateInsufficientPrivilege = "42501"
	sqlStateLockNotAvailable      = "55P03"
)

// Settings carry everything the catalog needs to reach PostgreSQL.
type Settings struct {
	SchemaName string
	DSN        string
}

// SettingsFromConfig resolves the DSN without ever copying it into an error.
func SettingsFromConfig(search config.Search) (Settings, error) {
	if search.DSN != "" {
		return Settings{
			SchemaName: search.SchemaName,
			DSN:        search.DSN,
		}, nil
	}
	value := strings.TrimSpace(os.Getenv(search.DSNEnv))
	if value == "" {
		return Settings{}, &Error{
			Kind: KindPostgresNotConfigured,
			Message: fmt.Sprintf(
				"environment variable %s named by search.dsn_env is empty",
				search.DSNEnv,
			),
			Remediation: fmt.Sprintf(
				"export %s with the PostgreSQL connection URL",
				search.DSNEnv,
			),
			Details: map[string]any{"dsn_env": search.DSNEnv},
		}
	}
	return Settings{SchemaName: search.SchemaName, DSN: value}, nil
}

// Catalog opens its pool lazily; reader commands never construct one.
type Catalog struct {
	settings Settings
	schema   string
	mutex    sync.Mutex
	pool     *pgxpool.Pool
	// publishHook runs inside Publish before commit so tests can interrupt it.
	publishHook func(context.Context) error
}

func New(settings Settings) (*Catalog, error) {
	if err := ValidateSchemaName(settings.SchemaName); err != nil {
		return nil, err
	}
	if settings.DSN == "" {
		return nil, &Error{
			Kind:        KindPostgresNotConfigured,
			Message:     "no PostgreSQL connection URL was configured",
			Remediation: "set dsn_env or dsn under [search] in the configuration",
		}
	}
	return &Catalog{
		settings: settings,
		schema:   quoteIdentifier(settings.SchemaName),
	}, nil
}

// SchemaName returns the configured catalog schema.
func (catalog *Catalog) SchemaName() string {
	return catalog.settings.SchemaName
}

func (catalog *Catalog) Close() {
	catalog.mutex.Lock()
	defer catalog.mutex.Unlock()
	if catalog.pool != nil {
		catalog.pool.Close()
		catalog.pool = nil
	}
}

func (catalog *Catalog) acquire(ctx context.Context) (*pgxpool.Pool, error) {
	catalog.mutex.Lock()
	defer catalog.mutex.Unlock()
	if catalog.pool != nil {
		return catalog.pool, nil
	}
	poolConfig, err := pgxpool.ParseConfig(catalog.settings.DSN)
	if err != nil {
		// The driver error quotes the DSN, so only the config field is named.
		return nil, &Error{
			Kind: KindPostgresNotConfigured,
			Message: "the configured PostgreSQL connection URL is not a" +
				" valid libpq or URL connection string",
			Remediation: "fix the value named by search.dsn_env or search.dsn",
			Cause:       err,
		}
	}
	poolConfig.MaxConns = maxConnections
	poolConfig.ConnConfig.ConnectTimeout = connectTimeout
	parameters := poolConfig.ConnConfig.RuntimeParams
	parameters["statement_timeout"] = statementTimeout
	parameters["lock_timeout"] = lockTimeout
	// Extension types and operators resolve through the search path.
	parameters["search_path"] = catalog.schema + ", public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, catalog.unreachable(poolConfig.ConnConfig, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, catalog.unreachable(poolConfig.ConnConfig, err)
	}
	catalog.pool = pool
	return pool, nil
}

func (catalog *Catalog) unreachable(
	connection *pgx.ConnConfig,
	err error,
) error {
	endpoint := net.JoinHostPort(
		connection.Host,
		strconv.FormatUint(uint64(connection.Port), 10),
	)
	return &Error{
		Kind: KindPostgresUnreachable,
		Message: fmt.Sprintf(
			"PostgreSQL at %s did not accept a connection (%s)",
			endpoint,
			causeClass(err),
		),
		Remediation: "start PostgreSQL, for example with" +
			" docker compose -f postgres/compose.yaml up -d --wait," +
			" and confirm the configured host, port, and credentials",
		Details: map[string]any{
			"endpoint":    endpoint,
			"cause_class": causeClass(err),
		},
		Cause: err,
	}
}

// causeClass classifies a connection failure without echoing any DSN part.
func causeClass(err error) string {
	var serverError *pgconn.PgError
	if errors.As(err, &serverError) {
		switch {
		case strings.HasPrefix(serverError.Code, "28"):
			return "authentication_rejected"
		case strings.HasPrefix(serverError.Code, "3D"):
			return "database_missing"
		case strings.HasPrefix(serverError.Code, "08"):
			return "connection_rejected"
		default:
			return "server_error"
		}
	}
	var resolution *net.DNSError
	if errors.As(err, &resolution) {
		return "dns_resolution_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "connect_timeout"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "connect_timeout"
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return "dial_failed"
	}
	return "connect_failed"
}

// checkPostgresMajor is the pure version gate used before any schema work.
func checkPostgresMajor(serverVersionNum int) (int, error) {
	major := serverVersionNum / 10000
	if major == SupportedPostgresMajor {
		return major, nil
	}
	return major, &Error{
		Kind: KindPostgresVersionUnsupported,
		Message: fmt.Sprintf(
			"PostgreSQL major %d is unsupported, sessionio requires major %d",
			major,
			SupportedPostgresMajor,
		),
		Remediation: "connect sessionio to a PostgreSQL 18 endpoint such as" +
			" the project compose profile in postgres/compose.yaml",
		Details: map[string]any{
			"found_major":    major,
			"expected_major": SupportedPostgresMajor,
		},
	}
}

type extensionRequirement struct {
	name    string
	version string
}

// requiredExtensions pins the canonical profile; pg_trgm accepts any version.
var requiredExtensions = []extensionRequirement{
	{name: "vector", version: "0.8.5"},
	{name: "pg_textsearch", version: "1.3.1"},
	{name: "pg_trgm"},
}

type extensionState struct {
	Installed string
	Available string
}

type probeFacts struct {
	User              string
	Database          string
	ServerVersionNum  int
	PreloadReadable   bool
	PreloadLibraries  string
	LibraryVersion    string
	VectorTypeVisible bool
	CanCreateDatabase bool
	SchemaExists      bool
	SchemaUsable      bool
	Extensions        map[string]extensionState
}

const probeQuery = `SELECT current_user,
	current_database(),
	current_setting('server_version_num')::int,
	(SELECT setting FROM pg_settings WHERE name = 'shared_preload_libraries'),
	current_setting('pg_textsearch.library_version', true),
	to_regtype('vector') IS NOT NULL,
	has_database_privilege(current_database(), 'CREATE'),
	to_regnamespace($1) IS NOT NULL`

const extensionQuery = `SELECT requested.name,
	COALESCE(installed.extversion, ''),
	COALESCE(available.default_version, '')
FROM unnest($1::text[]) AS requested(name)
LEFT JOIN pg_extension installed ON installed.extname = requested.name
LEFT JOIN pg_available_extensions available ON available.name = requested.name`

const schemaPrivilegeQuery = `SELECT has_schema_privilege($1, 'USAGE')
	AND has_schema_privilege($1, 'CREATE')`

func (catalog *Catalog) probe(
	ctx context.Context,
	connection queryRunner,
) (probeFacts, error) {
	facts := probeFacts{Extensions: map[string]extensionState{}}
	var preload *string
	var libraryVersion *string
	if err := connection.QueryRow(
		ctx,
		probeQuery,
		catalog.settings.SchemaName,
	).Scan(
		&facts.User,
		&facts.Database,
		&facts.ServerVersionNum,
		&preload,
		&libraryVersion,
		&facts.VectorTypeVisible,
		&facts.CanCreateDatabase,
		&facts.SchemaExists,
	); err != nil {
		return probeFacts{}, fmt.Errorf("probe PostgreSQL capabilities: %w", err)
	}
	if preload != nil {
		facts.PreloadReadable = true
		facts.PreloadLibraries = *preload
	}
	if libraryVersion != nil {
		facts.LibraryVersion = *libraryVersion
	}
	names := make([]string, 0, len(requiredExtensions))
	for _, requirement := range requiredExtensions {
		names = append(names, requirement.name)
	}
	rows, err := connection.Query(ctx, extensionQuery, names)
	if err != nil {
		return probeFacts{}, fmt.Errorf("probe PostgreSQL extensions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var state extensionState
		if err := rows.Scan(&name, &state.Installed, &state.Available); err != nil {
			return probeFacts{}, fmt.Errorf("read extension state: %w", err)
		}
		facts.Extensions[name] = state
	}
	if err := rows.Err(); err != nil {
		return probeFacts{}, fmt.Errorf("read extension state: %w", err)
	}
	if facts.SchemaExists {
		if err := connection.QueryRow(
			ctx,
			schemaPrivilegeQuery,
			catalog.settings.SchemaName,
		).Scan(&facts.SchemaUsable); err != nil {
			return probeFacts{}, fmt.Errorf("probe schema privileges: %w", err)
		}
	}
	return facts, nil
}

// preloaded reports whether pg_textsearch was preloaded by the server.
func preloaded(facts probeFacts) bool {
	if facts.PreloadReadable {
		for _, library := range strings.Split(facts.PreloadLibraries, ",") {
			if strings.TrimSpace(library) == "pg_textsearch" {
				return true
			}
		}
		return false
	}
	// Non-superusers cannot read shared_preload_libraries; this GUC exists
	// only when pg_textsearch was preloaded.
	return facts.LibraryVersion != ""
}

type capabilityProblem struct {
	Item        string
	Detail      string
	Installable bool
}

func capabilityProblems(facts probeFacts) []capabilityProblem {
	var problems []capabilityProblem
	if !preloaded(facts) {
		problems = append(problems, capabilityProblem{
			Item: "shared_preload_libraries",
			Detail: "pg_textsearch is absent from shared_preload_libraries;" +
				" preload it and restart PostgreSQL",
		})
	}
	for _, requirement := range requiredExtensions {
		if problem, failed := extensionProblem(facts, requirement); failed {
			problems = append(problems, problem)
		}
	}
	return problems
}

func extensionProblem(
	facts probeFacts,
	requirement extensionRequirement,
) (capabilityProblem, bool) {
	item := "extension_" + requirement.name
	state := facts.Extensions[requirement.name]
	if state.Installed == "" {
		if state.Available == "" {
			return capabilityProblem{
				Item: item,
				Detail: fmt.Sprintf(
					"extension %s is neither installed nor available",
					requirement.name,
				),
			}, true
		}
		if requirement.version != "" && state.Available != requirement.version {
			return capabilityProblem{
				Item: item,
				Detail: fmt.Sprintf(
					"extension %s offers version %s, %s is required",
					requirement.name,
					state.Available,
					requirement.version,
				),
			}, true
		}
		return capabilityProblem{
			Item: item,
			Detail: fmt.Sprintf(
				"extension %s is available but not installed",
				requirement.name,
			),
			Installable: true,
		}, true
	}
	if requirement.version != "" && state.Installed != requirement.version {
		return capabilityProblem{
			Item: item,
			Detail: fmt.Sprintf(
				"extension %s version %s is installed, %s is required",
				requirement.name,
				state.Installed,
				requirement.version,
			),
		}, true
	}
	if requirement.name == "vector" && !facts.VectorTypeVisible {
		return capabilityProblem{
			Item: item,
			Detail: "extension vector is installed but its type is not" +
				" visible on the connection search path",
		}, true
	}
	return capabilityProblem{}, false
}

func capabilityMissing(problems []capabilityProblem) error {
	details := make(map[string]any, len(problems))
	items := make([]string, 0, len(problems))
	for _, problem := range problems {
		details[problem.Item] = problem.Detail
		items = append(items, problem.Item)
	}
	return &Error{
		Kind: KindPostgresCapabilityMissing,
		Message: fmt.Sprintf(
			"PostgreSQL is missing required capabilities: %s",
			strings.Join(items, ", "),
		),
		Remediation: "start the canonical profile in postgres/compose.yaml or" +
			" install the pinned pg_textsearch, pgvector, and pg_trgm" +
			" extensions into the configured database",
		Details: details,
	}
}

func privilegeMissing(action string, err error) error {
	return &Error{
		Kind: KindPostgresCapabilityMissing,
		Message: fmt.Sprintf(
			"the connected role is not allowed to %s",
			action,
		),
		Remediation: "grant the role CREATE on the database, or run" +
			" catalog init with a role that owns the catalog schema",
		Details: map[string]any{"denied_action": action},
		Cause:   err,
	}
}

// InitResult is the machine record of one catalog initialization.
type InitResult struct {
	PostgresMajor   int
	CatalogSchema   string
	CatalogRevision int
	Created         bool
}

func (catalog *Catalog) Init(ctx context.Context) (InitResult, error) {
	pool, err := catalog.acquire(ctx)
	if err != nil {
		return InitResult{}, err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return InitResult{}, fmt.Errorf("acquire PostgreSQL connection: %w", err)
	}
	defer connection.Release()
	facts, err := catalog.probe(ctx, connection)
	if err != nil {
		return InitResult{}, err
	}
	major, err := checkPostgresMajor(facts.ServerVersionNum)
	if err != nil {
		return InitResult{}, err
	}
	if err := catalog.ensureCapabilities(ctx, connection, facts); err != nil {
		return InitResult{}, err
	}
	created, err := catalog.initializeSchema(ctx, connection)
	if err != nil {
		return InitResult{}, err
	}
	return InitResult{
		PostgresMajor:   major,
		CatalogSchema:   catalog.settings.SchemaName,
		CatalogRevision: Revision,
		Created:         created,
	}, nil
}

func (catalog *Catalog) ensureCapabilities(
	ctx context.Context,
	connection queryRunner,
	facts probeFacts,
) error {
	problems := capabilityProblems(facts)
	if len(problems) == 0 {
		return nil
	}
	var blocking []capabilityProblem
	var installable []capabilityProblem
	for _, problem := range problems {
		if problem.Installable {
			installable = append(installable, problem)
			continue
		}
		blocking = append(blocking, problem)
	}
	if len(blocking) > 0 {
		return capabilityMissing(blocking)
	}
	for _, problem := range installable {
		name := strings.TrimPrefix(problem.Item, "extension_")
		statement := fmt.Sprintf(
			"CREATE EXTENSION IF NOT EXISTS %s SCHEMA public",
			quoteIdentifier(name),
		)
		if _, err := connection.Exec(ctx, statement); err != nil {
			if isSQLState(err, sqlStateInsufficientPrivilege) {
				return privilegeMissing(
					"create extension "+name+" in the database",
					err,
				)
			}
			return fmt.Errorf("create extension %s: %w", name, err)
		}
	}
	refreshed, err := catalog.probe(ctx, connection)
	if err != nil {
		return err
	}
	if remaining := capabilityProblems(refreshed); len(remaining) > 0 {
		return capabilityMissing(remaining)
	}
	return nil
}

func (catalog *Catalog) initializeSchema(
	ctx context.Context,
	connection queryRunner,
) (created bool, err error) {
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin catalog initialization: %w", err)
	}
	defer func() {
		err = errors.Join(err, discard(ctx, transaction))
	}()
	if _, err := transaction.Exec(
		ctx,
		"SELECT pg_advisory_xact_lock(hashtext($1), $2)",
		"sessionio:"+catalog.settings.SchemaName,
		int32(advisoryLockKey),
	); err != nil {
		return false, fmt.Errorf("lock catalog maintenance: %w", err)
	}
	if _, err := transaction.Exec(
		ctx,
		"CREATE SCHEMA IF NOT EXISTS "+catalog.schema,
	); err != nil {
		if isSQLState(err, sqlStateInsufficientPrivilege) {
			return false, privilegeMissing(
				"create schema "+catalog.settings.SchemaName,
				err,
			)
		}
		return false, fmt.Errorf("create catalog schema: %w", err)
	}
	created, err = catalog.ensureSubstrate(ctx, transaction)
	if err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit catalog initialization: %w", err)
	}
	return created, nil
}

func (catalog *Catalog) ensureSubstrate(
	ctx context.Context,
	transaction pgx.Tx,
) (bool, error) {
	present, err := catalog.tableExists(ctx, transaction, "catalog_meta")
	if err != nil {
		return false, err
	}
	if !present {
		for _, statement := range substrateStatements(catalog.schema) {
			if _, err := transaction.Exec(ctx, statement); err != nil {
				if isSQLState(err, sqlStateInsufficientPrivilege) {
					return false, privilegeMissing(
						"create catalog tables in schema "+
							catalog.settings.SchemaName,
						err,
					)
				}
				return false, fmt.Errorf("create catalog substrate: %w", err)
			}
		}
		if _, err := transaction.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.catalog_meta (revision, initialized_at)"+
				" VALUES ($1, now())",
			catalog.schema,
		), Revision); err != nil {
			return false, fmt.Errorf("record catalog revision: %w", err)
		}
		return true, nil
	}
	var found int
	if err := transaction.QueryRow(ctx, fmt.Sprintf(
		"SELECT revision FROM %s.catalog_meta",
		catalog.schema,
	)).Scan(&found); err != nil {
		return false, fmt.Errorf("read catalog revision: %w", err)
	}
	if found != Revision {
		return false, revisionMismatch(catalog.settings.SchemaName, found)
	}
	for _, table := range substrateTables {
		exists, err := catalog.tableExists(ctx, transaction, table)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, &Error{
				Kind: KindCatalogSchemaMismatch,
				Message: fmt.Sprintf(
					"catalog revision %d is recorded but table %s is missing",
					found,
					table,
				),
				Remediation: "reset the pre-freeze catalog: export retained" +
					" state, drop the schema, and run sessionio catalog init",
				Details: map[string]any{
					"catalog_schema": catalog.settings.SchemaName,
					"missing_table":  table,
				},
			}
		}
	}
	return false, nil
}

func revisionMismatch(schemaName string, found int) error {
	return &Error{
		Kind: KindCatalogSchemaMismatch,
		Message: fmt.Sprintf(
			"catalog schema %s carries revision %d, this build requires %d",
			schemaName,
			found,
			Revision,
		),
		Remediation: "the project is pre-contract-freeze and keeps no" +
			" migration chain: export retained state, drop the schema, and" +
			" run sessionio catalog init",
		Details: map[string]any{
			"catalog_schema": schemaName,
			"found":          found,
			"expected":       Revision,
		},
	}
}

func (catalog *Catalog) tableExists(
	ctx context.Context,
	connection queryRunner,
	table string,
) (bool, error) {
	var exists bool
	if err := connection.QueryRow(
		ctx,
		"SELECT to_regclass($1) IS NOT NULL",
		catalog.schema+"."+quoteIdentifier(table),
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up table %s: %w", table, err)
	}
	return exists, nil
}

func isSQLState(err error, state string) bool {
	var serverError *pgconn.PgError
	if errors.As(err, &serverError) {
		return serverError.Code == state
	}
	return false
}

// queryRunner is satisfied by pgxpool connections and transactions.
type queryRunner interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
