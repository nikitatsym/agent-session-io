//go:build pgintegration

package catalog

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestInitIsIdempotentAndDetectsRevisionDrift(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	first := mustInit(t, catalog)
	if !first.Created {
		t.Fatal("first init reported created=false on an empty schema")
	}
	if first.PostgresMajor != SupportedPostgresMajor {
		t.Fatalf(
			"postgres major = %d, want %d",
			first.PostgresMajor,
			SupportedPostgresMajor,
		)
	}
	if first.CatalogRevision != Revision {
		t.Fatalf(
			"catalog revision = %d, want %d",
			first.CatalogRevision,
			Revision,
		)
	}
	second := mustInit(t, catalog)
	if second.Created {
		t.Fatal("second init reported created=true")
	}
	mustExec(t, catalog, fmt.Sprintf(
		"UPDATE %s.catalog_meta SET revision = 999",
		catalog.schema,
	))
	_, err := catalog.Init(context.Background())
	typed := requireKind(t, err, KindCatalogSchemaMismatch)
	if typed.Details["found"] != 999 {
		t.Fatalf("details = %v, want found 999", typed.Details)
	}
	if typed.Details["expected"] != Revision {
		t.Fatalf("details = %v, want expected %d", typed.Details, Revision)
	}
}

func TestProbeAcceptsTheConfiguredEndpoint(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	ctx := context.Background()
	pool, err := catalog.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire pool: %v", err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer connection.Release()
	facts, err := catalog.probe(ctx, connection)
	if err != nil {
		t.Fatalf("probe capabilities: %v", err)
	}
	major, err := checkPostgresMajor(facts.ServerVersionNum)
	if err != nil {
		t.Fatalf("version gate rejected the endpoint: %v", err)
	}
	if major != SupportedPostgresMajor {
		t.Fatalf("major = %d, want %d", major, SupportedPostgresMajor)
	}
	if !preloaded(facts) {
		t.Fatalf(
			"pg_textsearch is not preloaded (readable=%t libraries=%q version=%q)",
			facts.PreloadReadable,
			facts.PreloadLibraries,
			facts.LibraryVersion,
		)
	}
	if problems := capabilityProblems(facts); len(problems) != 0 {
		t.Fatalf("capability problems = %v, want none", problems)
	}
	if !facts.VectorTypeVisible {
		t.Fatal("the vector type is not visible on the search path")
	}
	for parameter, want := range map[string]string{
		"statement_timeout": "30s",
		"lock_timeout":      "30s",
	} {
		var setting string
		if err := connection.QueryRow(
			ctx,
			"SELECT current_setting($1)",
			parameter,
		).Scan(&setting); err != nil {
			t.Fatalf("read %s: %v", parameter, err)
		}
		if setting != want {
			t.Fatalf("%s = %q, want %q", parameter, setting, want)
		}
	}
}

func TestStatusReportsCatalogNotInitialized(t *testing.T) {
	catalog := newTestCatalog(t, testEndpoint(t, primaryEndpointEnv))
	ctx := context.Background()
	_, err := catalog.Status(ctx)
	requireKind(t, err, KindCatalogNotInitialized)
	mustExec(t, catalog, "CREATE SCHEMA "+catalog.schema)
	_, err = catalog.Status(ctx)
	typed := requireKind(t, err, KindCatalogNotInitialized)
	if typed.Remediation != "sessionio catalog init" {
		t.Fatalf(
			"remediation = %q, want sessionio catalog init",
			typed.Remediation,
		)
	}
	mustInit(t, catalog)
	status, err := catalog.Status(ctx)
	if err != nil {
		t.Fatalf("status after init: %v", err)
	}
	if !status.SchemaExists || status.Revision != Revision {
		t.Fatalf("status = %+v, want the current revision", status)
	}
}

func TestInitWithoutPrivilegesReportsCapabilityMissing(t *testing.T) {
	adminURL := testEndpoint(t, adminEndpointEnv)
	role := fmt.Sprintf("sessionio_limited_%d", os.Getpid())
	const password = "sessionio-limited"
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the admin endpoint: %v", err)
	}
	// Registered first so it closes after the role has been dropped.
	t.Cleanup(func() { closeConnection(t, admin) })
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB",
		quoteIdentifier(role),
		password,
	)); err != nil {
		t.Fatalf("create limited role: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(
			context.Background(),
			"DROP ROLE IF EXISTS "+quoteIdentifier(role),
		); err != nil {
			t.Errorf("drop limited role: %v", err)
		}
	})
	var canCreate bool
	if err := admin.QueryRow(
		ctx,
		"SELECT has_database_privilege($1, current_database(), 'CREATE')",
		role,
	).Scan(&canCreate); err != nil {
		t.Fatalf("read database privileges: %v", err)
	}
	if canCreate {
		t.Fatalf("role %s unexpectedly holds CREATE on the database", role)
	}
	limited := newTestCatalog(t, limitedDSN(t, adminURL, role, password))
	_, err = limited.Init(ctx)
	typed := requireKind(t, err, KindPostgresCapabilityMissing)
	if typed.Details["denied_action"] == nil {
		t.Fatalf("details = %v, want the denied action", typed.Details)
	}
}

func limitedDSN(t *testing.T, dsn string, role string, password string) string {
	t.Helper()
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the admin endpoint: %v", err)
	}
	limited := url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(role, password),
		Host:   fmt.Sprintf("%s:%d", parsed.Host, parsed.Port),
		Path:   "/" + parsed.Database,
	}
	return limited.String()
}
