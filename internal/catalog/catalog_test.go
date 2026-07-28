package catalog

import (
	"errors"
	"strings"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/config"
)

func TestCheckPostgresMajorGate(t *testing.T) {
	cases := []struct {
		name          string
		serverVersion int
		wantMajor     int
		wantKind      Kind
	}{
		{name: "postgres 17", serverVersion: 170006, wantMajor: 17,
			wantKind: KindPostgresVersionUnsupported},
		{name: "postgres 18", serverVersion: 180004, wantMajor: 18},
		{name: "postgres 19", serverVersion: 190001, wantMajor: 19,
			wantKind: KindPostgresVersionUnsupported},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			major, err := checkPostgresMajor(testCase.serverVersion)
			if major != testCase.wantMajor {
				t.Fatalf("major = %d, want %d", major, testCase.wantMajor)
			}
			if testCase.wantKind == "" {
				if err != nil {
					t.Fatalf("version gate rejected %d: %v",
						testCase.serverVersion, err)
				}
				return
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("error = %v, want *catalog.Error", err)
			}
			if typed.Kind != testCase.wantKind {
				t.Fatalf("kind = %q, want %q", typed.Kind, testCase.wantKind)
			}
			if typed.Details["found_major"] != testCase.wantMajor {
				t.Fatalf("details = %v, want found_major %d",
					typed.Details, testCase.wantMajor)
			}
			if typed.Remediation == "" {
				t.Fatal("version failure carries no remediation")
			}
		})
	}
}

func TestValidateSchemaName(t *testing.T) {
	valid := []string{"sessionio", "_private", "sessionio_it_1234_7"}
	for _, name := range valid {
		if err := ValidateSchemaName(name); err != nil {
			t.Fatalf("schema name %q was rejected: %v", name, err)
		}
	}
	invalid := []string{
		"",
		"Bad-Name",
		"1session",
		"session\"io",
		"public; DROP SCHEMA public",
		strings.Repeat("a", 64),
	}
	for _, name := range invalid {
		err := ValidateSchemaName(name)
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("schema name %q was accepted", name)
		}
		if typed.Kind != KindConfigInvalid {
			t.Fatalf("kind = %q, want %q", typed.Kind, KindConfigInvalid)
		}
	}
}

func TestQuoteIdentifierEscapesQuotes(t *testing.T) {
	cases := map[string]string{
		"sessionio":     `"sessionio"`,
		"catalog_meta":  `"catalog_meta"`,
		`weird"name`:    `"weird""name"`,
		"search_facet1": `"search_facet1"`,
	}
	for input, want := range cases {
		if got := quoteIdentifier(input); got != want {
			t.Fatalf("quoteIdentifier(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDerivedStatementsQualifyTheConfiguredSchema(t *testing.T) {
	statements := derivedStatements(quoteIdentifier("sessionio_it"))
	joined := strings.Join(statements, "\n")
	for _, statement := range statements {
		if !strings.Contains(statement, `"sessionio_it".`) {
			t.Fatalf("statement is not schema qualified:\n%s", statement)
		}
	}
	for _, table := range derivedTables {
		if !strings.Contains(joined, quoteIdentifier(table)) {
			t.Fatalf("derived table %s is never created:\n%s", table, joined)
		}
		// A derived table is shared: a per-generation name would reintroduce
		// the row copying this schema exists to remove.
		if strings.Contains(table, "_g") {
			t.Fatalf("derived table %s is still per generation", table)
		}
	}
}

// A relation target kind is written by the reader and resolved by the catalog,
// so the two vocabularies must name the same thing.
func TestTheNativeRecordTargetMatchesTheReaderNodeKind(t *testing.T) {
	if ToKindNativeRecord != string(sessionio.NodeKindNativeRecord) {
		t.Fatalf(
			"catalog target %q does not match reader node kind %q",
			ToKindNativeRecord,
			sessionio.NodeKindNativeRecord,
		)
	}
	statements := derivedStatements(quoteIdentifier("sessionio_it"))
	joined := strings.Join(statements, "\n")
	if !strings.Contains(joined, "native_key") {
		t.Fatalf("no derived column retains the native key:\n%s", joined)
	}
	if !strings.Contains(joined, "(native_key) WHERE native_key <> ''") {
		t.Fatalf("the native key has no supporting index:\n%s", joined)
	}
	if !strings.Contains(relationResolutionQuery, "native_key") {
		t.Fatal("relation resolution does not use the native key")
	}
}

func TestBuilderKeyIsCanonical(t *testing.T) {
	key := BuilderKey(map[string]string{
		"projection": "sessionio.projection/v2",
		"passage":    "sessionio.passage/v2",
	})
	want := "passage=sessionio.passage/v2;projection=sessionio.projection/v2"
	if key != want {
		t.Fatalf("builder key = %q, want %q", key, want)
	}
	if BuilderKey(map[string]string{"passage": "sessionio.passage/v3"}) == key {
		t.Fatal("a builder bump produced the same key")
	}
}

func TestSettingsFromConfigReportsUnsetEnvironment(t *testing.T) {
	t.Setenv("SESSIONIO_TEST_UNSET_DSN", "")
	_, err := SettingsFromConfig(config.Search{
		Backend:    config.BackendPostgres,
		DSNEnv:     "SESSIONIO_TEST_UNSET_DSN",
		SchemaName: "sessionio",
	})
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want *catalog.Error", err)
	}
	if typed.Kind != KindPostgresNotConfigured {
		t.Fatalf("kind = %q, want %q", typed.Kind, KindPostgresNotConfigured)
	}
	if typed.Details["dsn_env"] != "SESSIONIO_TEST_UNSET_DSN" {
		t.Fatalf("details = %v, want the environment variable name",
			typed.Details)
	}
}

func TestSettingsFromConfigKeepsTheLiteralDSNOutOfErrors(t *testing.T) {
	settings, err := SettingsFromConfig(config.Search{
		Backend:    config.BackendPostgres,
		DSN:        "postgresql://sessionio:secret@127.0.0.1/sessionio",
		SchemaName: "sessionio",
	})
	if err != nil {
		t.Fatalf("resolve settings: %v", err)
	}
	if settings.DSN == "" {
		t.Fatal("settings carry no DSN")
	}
	_, err = New(Settings{SchemaName: "Bad-Name", DSN: settings.DSN})
	if err == nil {
		t.Fatal("invalid schema name was accepted")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks the DSN: %v", err)
	}
}
