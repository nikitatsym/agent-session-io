package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
)

// listingFixture drives the real command tree over the repository reader
// fixtures with an advisory cache directory of its own.
type listingFixture struct {
	t          *testing.T
	configPath string
	cacheDir   string
}

// newListingFixture lists the repository reader fixtures, which carry the
// diagnostics and odd records a retained entry must reproduce.
func newListingFixture(t *testing.T) *listingFixture {
	t.Helper()
	testdata, err := filepath.Abs(filepath.Join("..", "..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return newListingFixtureOver(
		t,
		filepath.Join(testdata, "codex"),
		filepath.Join(testdata, "claude"),
	)
}

// newReadableListingFixture lists a corpus every session of which can be read
// to the end, which a bounded query needs to resolve activity.
func newReadableListingFixture(t *testing.T) *listingFixture {
	t.Helper()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	rollout := filepath.Join(
		codexHome, "sessions", "2026", "07", "28",
		"rollout-2026-07-28T09-00-00-c0000000-0000-4000-8000-000000000001.jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	records := strings.Join([]string{
		`{"timestamp":"2026-07-28T09:00:00Z","type":"session_meta","payload":` +
			`{"id":"c0000000-0000-4000-8000-000000000001",` +
			`"cwd":"/workspace","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-28T09:00:05Z","type":"response_item","payload":` +
			`{"type":"message","role":"user","content":` +
			`[{"type":"input_text","text":"activity resolution"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(records), 0o644); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(root, "claude")
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	return newListingFixtureOver(t, codexHome, claudeDir)
}

func newListingFixtureOver(
	t *testing.T,
	codexHome string,
	claudeDir string,
) *listingFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &listingFixture{
		t:          t,
		configPath: filepath.Join(root, "config.toml"),
		cacheDir:   filepath.Join(root, "cache"),
	}
	// Literal strings: a TOML basic string would eat a Windows separator.
	config := fmt.Sprintf(
		"schema = \"sessionio.config/v1\"\n\n"+
			"[sources.codex]\nhome = '%s'\n\n"+
			"[sources.claude]\nconfig_dir = '%s'\n\n"+
			"[cache]\ndir = '%s'\n",
		codexHome,
		claudeDir,
		fixture.cacheDir,
	)
	if err := os.WriteFile(fixture.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// runRootCommand executes the real command tree against one configuration
// file, exactly as one invocation of the binary does.
func runRootCommand(
	configPath string,
	arguments ...string,
) (string, string, error) {
	root := newRoot(buildinfo.Info{Version: "0.0.0-test"}, rootOptions{
		newRegistry:          newDefaultRegistry,
		newPresenceProviders: newDefaultPresenceProviders,
	})
	var output bytes.Buffer
	var diagnostic bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&diagnostic)
	root.SetArgs(append([]string{"--config", configPath}, arguments...))
	err := root.ExecuteContext(context.Background())
	return output.String(), diagnostic.String(), err
}

func (fixture *listingFixture) run(arguments ...string) (string, string) {
	fixture.t.Helper()
	output, diagnostic, err := runRootCommand(fixture.configPath, arguments...)
	if err != nil {
		fixture.t.Fatalf("%v: %v\n%s", arguments, err, diagnostic)
	}
	return output, diagnostic
}

func (fixture *listingFixture) cacheFiles() []string {
	fixture.t.Helper()
	entries, err := os.ReadDir(fixture.cacheDir)
	if err != nil {
		fixture.t.Fatalf("read cache directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, filepath.Join(fixture.cacheDir, entry.Name()))
	}
	return names
}

// listingArguments covers every output contract a warm run must reproduce,
// including the bounded query that resolves activity by reading a session.
func listingArguments() [][]string {
	return [][]string{
		{"list", "--format", "ndjson"},
		{"list", "--format", "json"},
		{"list"},
		{"list", "--harness", "claude", "--format", "ndjson"},
	}
}

func TestAWarmListingIsByteIdenticalInEveryFormat(t *testing.T) {
	fixture := newListingFixture(t)
	for _, arguments := range listingArguments() {
		coldOut, coldErr := fixture.run(arguments...)
		warmOut, warmErr := fixture.run(arguments...)
		if coldOut != warmOut {
			t.Fatalf("%v stdout differs\ncold: %s\nwarm: %s",
				arguments, coldOut, warmOut)
		}
		if coldErr != warmErr {
			t.Fatalf("%v stderr differs\ncold: %s\nwarm: %s",
				arguments, coldErr, warmErr)
		}
		if coldOut == "" {
			t.Fatalf("%v listed nothing", arguments)
		}
	}
	if len(fixture.cacheFiles()) != 2 {
		t.Fatalf("cache files = %v, want one per source", fixture.cacheFiles())
	}
}

// A listing record without an update time resolves its activity by reading the
// whole session, so the resolved value belongs to the retained entry too.
func TestAWarmBoundedListingReusesResolvedActivity(t *testing.T) {
	fixture := newReadableListingFixture(t)
	arguments := []string{"list", "--since", "36500d", "--format", "ndjson"}
	cold, _ := fixture.run(arguments...)
	if !strings.Contains(cold, `"updated_at":"2026-07-28T09:00:05Z"`) {
		t.Fatalf("cold listing resolved no activity: %s", cold)
	}
	warm, _ := fixture.run(arguments...)
	if warm != cold {
		t.Fatalf("bounded listing differs\ncold: %s\nwarm: %s", cold, warm)
	}
}

// The cache is advisory: an unusable file is reported and replaced, and the
// listing it could not serve is identical to the one that read the sources.
func TestACorruptCacheFileIsReportedAndReplaced(t *testing.T) {
	fixture := newListingFixture(t)
	cold, _ := fixture.run("list", "--format", "ndjson")
	for _, path := range fixture.cacheFiles() {
		if err := os.WriteFile(path, []byte("not a cache file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	discarded, diagnostic := fixture.run("list", "--format", "ndjson")
	if discarded != cold {
		t.Fatalf("a corrupt cache changed the listing\ncold: %s\nafter: %s",
			cold, discarded)
	}
	if !strings.Contains(diagnostic, "reader cache: discarded") {
		t.Fatalf("stderr = %q", diagnostic)
	}
	repaired, diagnostic := fixture.run("list", "--format", "ndjson")
	if repaired != cold || strings.Contains(diagnostic, "discarded") {
		t.Fatalf("the discarded file was not replaced: %q", diagnostic)
	}
}

// Selector resolution and completion list through the same cached path, so
// they answer identically warm and cold.
func TestSelectorsAndCompletionUseTheCachedListing(t *testing.T) {
	fixture := newListingFixture(t)
	listed, _ := fixture.run("list", "--format", "ndjson")
	selector := firstSessionDigest(t, listed)
	coldShow, _ := fixture.run("show", selector)
	warmShow, _ := fixture.run("show", selector)
	if coldShow != warmShow || coldShow == "" {
		t.Fatalf("show differs warm\ncold: %s\nwarm: %s", coldShow, warmShow)
	}
	coldCompletions, _ := fixture.run("__complete", "show", "")
	warmCompletions, _ := fixture.run("__complete", "show", "")
	if coldCompletions != warmCompletions || coldCompletions == "" {
		t.Fatalf("completion differs warm\ncold: %s\nwarm: %s",
			coldCompletions, warmCompletions)
	}
}

func firstSessionDigest(t *testing.T, listing string) string {
	t.Helper()
	const marker = `"id":"session:sha256:`
	index := strings.Index(listing, marker)
	if index < 0 {
		t.Fatalf("listing carries no session ID: %s", listing)
	}
	digest := listing[index+len(marker):]
	end := strings.IndexByte(digest, '"')
	if end < 0 {
		t.Fatalf("listing carries no session ID: %s", listing)
	}
	return digest[:end]
}
