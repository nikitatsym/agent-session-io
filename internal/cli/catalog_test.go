package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikitatsym/agent-session-io/internal/catalog"
)

func TestExitCodeForKind(t *testing.T) {
	cases := map[catalog.Kind]int{
		catalog.KindConfigInvalid:              exitInvalid,
		catalog.KindPostgresNotConfigured:      exitCapability,
		catalog.KindPostgresUnreachable:        exitCapability,
		catalog.KindPostgresVersionUnsupported: exitCapability,
		catalog.KindPostgresCapabilityMissing:  exitCapability,
		catalog.KindCatalogNotInitialized:      exitCapability,
		catalog.KindCatalogSchemaMismatch:      exitCapability,
		catalog.Kind("catalog_integrity_lost"): exitIntegrity,
	}
	for kind, want := range cases {
		if got := exitCodeForKind(kind); got != want {
			t.Fatalf("exit code for %q = %d, want %d", kind, got, want)
		}
	}
}

func TestTypedFailureWritesOneMachineRecord(t *testing.T) {
	var output bytes.Buffer
	err := typedFailure(&output, formatJSON, &catalog.Error{
		Kind:        catalog.KindCatalogNotInitialized,
		Message:     "catalog schema sessionio is not initialized",
		Remediation: "sessionio catalog init",
		Details:     map[string]any{"catalog_schema": "sessionio"},
	})
	if ExitCode(err) != exitCapability {
		t.Fatalf("exit code = %d, want %d", ExitCode(err), exitCapability)
	}
	if !ErrorReported(err) {
		t.Fatal("machine failure was not marked as reported")
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("machine output lines = %d, want 1\n%s", len(lines), output.String())
	}
	var record errorRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode error record: %v", err)
	}
	if record.Schema != errorSchema {
		t.Fatalf("schema = %q, want %q", record.Schema, errorSchema)
	}
	if record.Kind != string(catalog.KindCatalogNotInitialized) {
		t.Fatalf("kind = %q, want %q", record.Kind,
			catalog.KindCatalogNotInitialized)
	}
	if record.Remediation != "sessionio catalog init" {
		t.Fatalf("remediation = %q, want the init command", record.Remediation)
	}
}

func TestTypedFailureKeepsHumanOutputOnStderr(t *testing.T) {
	var output bytes.Buffer
	err := typedFailure(&output, formatHuman, &catalog.Error{
		Kind:    catalog.KindPostgresUnreachable,
		Message: "PostgreSQL at 127.0.0.1:1 did not accept a connection",
	})
	if output.Len() != 0 {
		t.Fatalf("human failure wrote stdout: %q", output.String())
	}
	if ErrorReported(err) {
		t.Fatal("human failure was marked as reported")
	}
	if ExitCode(err) != exitCapability {
		t.Fatalf("exit code = %d, want %d", ExitCode(err), exitCapability)
	}
}

func TestCatalogInitRejectsNDJSON(t *testing.T) {
	root, output, _ := testReaderRoot(t, time.Now)
	root.SetArgs([]string{
		"--config", "testdata/catalog/missing.toml",
		"catalog", "init", "--format", "ndjson",
	})
	err := root.Execute()
	if ExitCode(err) != exitInvalid {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), exitInvalid, err)
	}
	if !strings.Contains(err.Error(), "cannot stream") {
		t.Fatalf("error = %v, want a streaming complaint", err)
	}
	if output.Len() != 0 {
		t.Fatalf("rejected format wrote stdout: %q", output.String())
	}
}

func TestCatalogInitReportsInvalidConfiguration(t *testing.T) {
	record := runCatalogInitJSON(t, `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_TEST_ABSENT_URL"
schema_name = "Bad-Name"
`, exitInvalid)
	if record.Kind != string(catalog.KindConfigInvalid) {
		t.Fatalf("kind = %q, want %q", record.Kind, catalog.KindConfigInvalid)
	}
	if !strings.Contains(record.Message, "schema_name") {
		t.Fatalf("message = %q, want a schema_name complaint", record.Message)
	}
}

func TestCatalogInitReportsUnsetDSNEnvironment(t *testing.T) {
	t.Setenv("SESSIONIO_TEST_ABSENT_URL", "")
	record := runCatalogInitJSON(t, `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_TEST_ABSENT_URL"
schema_name = "sessionio"
`, exitCapability)
	if record.Kind != string(catalog.KindPostgresNotConfigured) {
		t.Fatalf("kind = %q, want %q", record.Kind,
			catalog.KindPostgresNotConfigured)
	}
	if record.Details["dsn_env"] != "SESSIONIO_TEST_ABSENT_URL" {
		t.Fatalf("details = %v, want the environment variable name",
			record.Details)
	}
}

func runCatalogInitJSON(
	t *testing.T,
	document string,
	wantExit int,
) errorRecord {
	t.Helper()
	path := writeFixtureFile(t, "config.toml", document)
	root, output, _ := testReaderRoot(t, time.Now)
	root.SetArgs([]string{
		"--config", path,
		"catalog", "init", "--format", "json",
	})
	err := root.Execute()
	if ExitCode(err) != wantExit {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), wantExit, err)
	}
	var record errorRecord
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode error record: %v\n%s", err, output.String())
	}
	return record
}

func TestDoctorRejectsUnknownScope(t *testing.T) {
	root, _, _ := testReaderRoot(t, time.Now)
	root.SetArgs([]string{"doctor", "--scope", "everything"})
	err := root.Execute()
	if ExitCode(err) != exitInvalid {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), exitInvalid, err)
	}
}

// A reader command must run with an unreadable --config, because it never
// loads the configuration file.
func TestReaderCommandIgnoresConfigFlag(t *testing.T) {
	garbage := writeFixtureFile(t, "garbage.toml", "this is not TOML {{{\n")
	root, output, diagnostic := testReaderRoot(t, time.Now)
	root.SetArgs([]string{"--config", garbage, "sources", "--format", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute sources with a garbage --config: %v", err)
	}
	if !strings.Contains(output.String(), "sessionio.reader/v1") {
		t.Fatalf("sources output = %q, want the reader contract",
			output.String())
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("sources wrote diagnostics: %q", diagnostic.String())
	}
}

func writeFixtureFile(t *testing.T, name string, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}
