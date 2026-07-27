package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/catalog"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

func TestExitCodeForKind(t *testing.T) {
	cases := map[catalog.Kind]int{
		catalog.KindConfigInvalid:               exitInvalid,
		catalog.KindSearchRequestInvalid:        exitInvalid,
		catalog.KindPostgresNotConfigured:       exitCapability,
		catalog.KindPostgresUnreachable:         exitCapability,
		catalog.KindPostgresVersionUnsupported:  exitCapability,
		catalog.KindPostgresCapabilityMissing:   exitCapability,
		catalog.KindCatalogNotInitialized:       exitCapability,
		catalog.KindCatalogSchemaMismatch:       exitCapability,
		catalog.KindCatalogGenerationIncomplete: exitCapability,
		catalog.Kind("catalog_integrity_lost"):  exitIntegrity,
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

// Every catalog-backed command emits one record, so none of them may accept
// the streaming format.
func TestCatalogCommandsRejectNDJSON(t *testing.T) {
	commands := [][]string{
		{"catalog", "init"},
		{"scan"},
		{"doctor"},
		{"search", "protocol"},
	}
	for _, command := range commands {
		root, output, _ := testReaderRoot(t, time.Now)
		args := append(
			[]string{"--config", "testdata/catalog/missing.toml"},
			command...,
		)
		root.SetArgs(append(args, "--format", "ndjson"))
		err := root.Execute()
		if ExitCode(err) != exitInvalid {
			t.Fatalf("%v exit code = %d, want %d (%v)",
				command, ExitCode(err), exitInvalid, err)
		}
		if !strings.Contains(err.Error(), "cannot stream") {
			t.Fatalf("%v error = %v, want a streaming complaint", command, err)
		}
		if output.Len() != 0 {
			t.Fatalf("%v rejected format wrote stdout: %q",
				command, output.String())
		}
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

// A reader command reads --config for its source roots, so an unparsable file
// is an invalid request instead of a silently ignored one.
func TestReaderCommandRejectsAnUnparsableConfig(t *testing.T) {
	garbage := writeFixtureFile(t, "garbage.toml", "this is not TOML {{{\n")
	root, output, _ := testReaderRoot(t, time.Now)
	root.SetArgs([]string{"--config", garbage, "sources", "--format", "json"})
	err := root.Execute()
	if ExitCode(err) != exitInvalid {
		t.Fatalf("exit code = %d, want %d (%v)", ExitCode(err), exitInvalid, err)
	}
	if output.Len() != 0 {
		t.Fatalf("rejected configuration wrote stdout: %q", output.String())
	}
}

// A configuration without [sources] leaves reader discovery exactly as it was.
func TestReaderCommandKeepsDiscoveryWithoutDeclaredSources(t *testing.T) {
	path := writeFixtureFile(t, "config.toml", `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_TEST_ABSENT_URL"
`)
	sources := runSourcesWithConfig(t, path)
	if sources.CodexHome() != "" || sources.ClaudeConfigDir() != "" {
		t.Fatalf("sources = %+v, want no declared root", sources)
	}
}

func TestDeclaredSourceRootsReachTheRegistry(t *testing.T) {
	path := writeFixtureFile(t, "config.toml", `schema = "sessionio.config/v1"

[sources.codex]
home = "codex-root"

[sources.claude]
config_dir = "claude-root"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_TEST_ABSENT_URL"
`)
	sources := runSourcesWithConfig(t, path)
	wantCodex := filepath.Join(filepath.Dir(path), "codex-root")
	wantClaude := filepath.Join(filepath.Dir(path), "claude-root")
	if sources.CodexHome() != wantCodex {
		t.Fatalf("codex home = %q, want %q", sources.CodexHome(), wantCodex)
	}
	if sources.ClaudeConfigDir() != wantClaude {
		t.Fatalf("claude config dir = %q, want %q",
			sources.ClaudeConfigDir(), wantClaude)
	}
}

func runSourcesWithConfig(t *testing.T, path string) config.Sources {
	t.Helper()
	var seen config.Sources
	root, _, _ := testRootWithRegistry(
		time.Now,
		nil,
		func(sources config.Sources) (*sessionio.Registry, error) {
			seen = sources
			return sessionio.NewRegistry(&fakeReaderAdapter{
				descriptor: testDescriptor(sessionio.HarnessCodex),
			})
		},
	)
	root.SetArgs([]string{"--config", path, "sources", "--format", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute sources with %s: %v", path, err)
	}
	return seen
}

func writeFixtureFile(t *testing.T, name string, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}
