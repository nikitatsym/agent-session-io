//go:build pgintegration

package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	primaryEndpointEnv = "SESSIONIO_TEST_DATABASE_URL"
	composeEndpointEnv = "SESSIONIO_TEST_COMPOSE_DATABASE_URL"
)

var schemaSequence atomic.Int64

func testEndpoint(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set for pgintegration tests", name)
	}
	return value
}

// newTestCatalog owns a unique schema and drops it during cleanup.
func newTestCatalog(t *testing.T, dsn string) *Catalog {
	t.Helper()
	schema := fmt.Sprintf(
		"sessionio_it_%d_%d",
		os.Getpid(),
		schemaSequence.Add(1),
	)
	catalog, err := New(Settings{SchemaName: schema, DSN: dsn})
	if err != nil {
		t.Fatalf("create catalog: %v", err)
	}
	t.Cleanup(func() {
		catalog.Close()
		dropSchema(t, dsn, schema)
	})
	return catalog
}

func dropSchema(t *testing.T, dsn string, schema string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to drop schema %s: %v", schema, err)
	}
	defer closeConnection(t, connection)
	if _, err := connection.Exec(
		ctx,
		"DROP SCHEMA IF EXISTS "+quoteIdentifier(schema)+" CASCADE",
	); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
}

func closeConnection(t *testing.T, connection *pgx.Conn) {
	t.Helper()
	if err := connection.Close(context.Background()); err != nil {
		t.Fatalf("close connection: %v", err)
	}
}

func mustInit(t *testing.T, catalog *Catalog) InitResult {
	t.Helper()
	result, err := catalog.Init(context.Background())
	if err != nil {
		t.Fatalf("initialize catalog: %v", err)
	}
	return result
}

func mustExec(t *testing.T, catalog *Catalog, statement string, args ...any) {
	t.Helper()
	ctx := context.Background()
	pool, err := catalog.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire pool: %v", err)
	}
	if _, err := pool.Exec(ctx, statement, args...); err != nil {
		t.Fatalf("execute %q: %v", statement, err)
	}
}

func queryInt(t *testing.T, catalog *Catalog, query string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := catalog.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire pool: %v", err)
	}
	var value int64
	if err := pool.QueryRow(ctx, query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// queryWithoutSequentialScans forces the index-backed plan for one query.
func queryWithoutSequentialScans(
	t *testing.T,
	catalog *Catalog,
	statement string,
	arguments []any,
	consume func(pgx.Rows),
) {
	t.Helper()
	ctx := context.Background()
	pool, err := catalog.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire pool: %v", err)
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin forced plan: %v", err)
	}
	defer func() {
		if err := discard(ctx, transaction); err != nil {
			t.Errorf("discard forced plan: %v", err)
		}
	}()
	if _, err := transaction.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	rows, err := transaction.Query(ctx, statement, arguments...)
	if err != nil {
		t.Fatalf("run %q: %v", statement, err)
	}
	defer rows.Close()
	consume(rows)
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", statement, err)
	}
}

func explain(t *testing.T, catalog *Catalog, query string, args ...any) string {
	t.Helper()
	plan := ""
	queryWithoutSequentialScans(
		t,
		catalog,
		"EXPLAIN "+query,
		args,
		func(rows pgx.Rows) {
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("read plan line: %v", err)
				}
				plan += line + "\n"
			}
		},
	)
	return plan
}

func requireKind(t *testing.T, err error, kind Kind) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("operation succeeded, want kind %q", kind)
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want *catalog.Error with kind %q", err, kind)
	}
	if typed.Kind != kind {
		t.Fatalf("kind = %q, want %q (%v)", typed.Kind, kind, err)
	}
	if typed.Remediation == "" {
		t.Fatalf("kind %q carries no remediation", kind)
	}
	return typed
}
