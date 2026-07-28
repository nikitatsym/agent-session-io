package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAcceptsMinimalConfiguration(t *testing.T) {
	loaded, _ := mustLoad(t, `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`)
	if loaded.Search.DSNEnv != "SESSIONIO_DATABASE_URL" {
		t.Fatalf("dsn_env = %q, want SESSIONIO_DATABASE_URL", loaded.Search.DSNEnv)
	}
	if loaded.Search.SchemaName != DefaultSchemaName {
		t.Fatalf(
			"schema_name = %q, want %q",
			loaded.Search.SchemaName,
			DefaultSchemaName,
		)
	}
}

func mustLoad(t *testing.T, document string) (*Config, string) {
	t.Helper()
	path := writeConfig(t, document)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	return loaded, path
}

func TestDeclaredSourceRootsResolveAgainstTheConfigurationFile(t *testing.T) {
	// filepath.IsAbs rejects "/absolute" on Windows, so build a real one.
	absolute := filepath.Join(t.TempDir(), "claude")
	loaded, path := mustLoad(t, `schema = "sessionio.config/v1"

[sources.codex]
home = "fixtures/codex"

[sources.claude]
config_dir = '`+absolute+`'

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`)
	want := filepath.Join(filepath.Dir(path), "fixtures", "codex")
	if loaded.Sources.CodexHome() != want {
		t.Fatalf("codex home = %q, want %q", loaded.Sources.CodexHome(), want)
	}
	if loaded.Sources.ClaudeConfigDir() != absolute {
		t.Fatalf("claude config dir = %q, want %q",
			loaded.Sources.ClaudeConfigDir(), absolute)
	}
}

func TestAbsentSourcesLeaveDiscoveryUnchanged(t *testing.T) {
	loaded, _ := mustLoad(t, `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`)
	if loaded.Sources.CodexHome() != "" ||
		loaded.Sources.ClaudeConfigDir() != "" {
		t.Fatalf("sources = %+v, want no declared root", loaded.Sources)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name     string
		document string
		field    string
		fragment string
	}{
		{
			name: "unknown top-level field",
			document: `schema = "sessionio.config/v1"
unexpected = 1

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`,
			field:    "unexpected",
			fragment: "unknown configuration field unexpected",
		},
		{
			name: "unknown search field",
			document: `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
unexpected_field = "x"
`,
			field:    "search.unexpected_field",
			fragment: "unknown configuration field search.unexpected_field",
		},
		{
			name: "unknown source harness",
			document: `schema = "sessionio.config/v1"

[sources.opencode]
home = "fixtures/opencode"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`,
			field:    "sources.opencode",
			fragment: "unknown configuration field sources.opencode",
		},
		{
			name: "unknown source field",
			document: `schema = "sessionio.config/v1"

[sources.codex]
root = "fixtures/codex"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`,
			field:    "sources.codex.root",
			fragment: "unknown configuration field sources.codex.root",
		},
		{
			name: "declared source root without a path",
			document: `schema = "sessionio.config/v1"

[sources.claude]

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`,
			field:    "sources.claude.config_dir",
			fragment: "sources.claude.config_dir is empty",
		},
		{
			name: "both dsn and dsn_env",
			document: `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
dsn = "postgresql://sessionio@127.0.0.1/sessionio"
`,
			field:    "search.dsn_env",
			fragment: "exactly one of dsn_env and dsn",
		},
		{
			name: "neither dsn nor dsn_env",
			document: `schema = "sessionio.config/v1"

[search]
backend = "postgres"
`,
			field:    "search.dsn_env",
			fragment: "exactly one of dsn_env and dsn",
		},
		{
			name: "invalid schema name",
			document: `schema = "sessionio.config/v1"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
schema_name = "Bad-Name"
`,
			field:    "search.schema_name",
			fragment: `search.schema_name "Bad-Name" is invalid`,
		},
		{
			name: "wrong schema version",
			document: `schema = "sessionio.config/v2"

[search]
backend = "postgres"
dsn_env = "SESSIONIO_DATABASE_URL"
`,
			field:    "schema",
			fragment: `schema "sessionio.config/v2" is unsupported`,
		},
		{
			name: "unsupported backend",
			document: `schema = "sessionio.config/v1"

[search]
backend = "sqlite"
dsn_env = "SESSIONIO_DATABASE_URL"
`,
			field:    "search.backend",
			fragment: `search.backend "sqlite" is unsupported`,
		},
		{
			name: "malformed TOML",
			document: `schema = "sessionio.config/v1"
[search
`,
			fragment: "invalid TOML",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeConfig(t, testCase.document)
			_, err := Load(path)
			configError := requireConfigError(t, err)
			if !strings.Contains(configError.Message, testCase.fragment) {
				t.Fatalf(
					"message = %q, want fragment %q",
					configError.Message,
					testCase.fragment,
				)
			}
			if testCase.field != "" && configError.Field != testCase.field {
				t.Fatalf(
					"field = %q, want %q",
					configError.Field,
					testCase.field,
				)
			}
			if configError.Remediation == "" {
				t.Fatal("configuration error carries no remediation")
			}
		})
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.toml")
	_, err := Load(path)
	configError := requireConfigError(t, err)
	if !strings.Contains(configError.Message, "does not exist") {
		t.Fatalf("message = %q, want a missing-file message", configError.Message)
	}
	if configError.Path != path {
		t.Fatalf("path = %q, want %q", configError.Path, path)
	}
}

func TestDefaultPathEndsInSessionioConfig(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("resolve default path: %v", err)
	}
	want := filepath.Join("sessionio", "config.toml")
	if !strings.HasSuffix(path, want) {
		t.Fatalf("default path = %q, want suffix %q", path, want)
	}
}

func requireConfigError(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("configuration was accepted, want a typed failure")
	}
	var configError *Error
	if !errors.As(err, &configError) {
		t.Fatalf("error = %v, want *config.Error", err)
	}
	return configError
}

func writeConfig(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	return path
}
