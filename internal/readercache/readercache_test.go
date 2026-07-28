package readercache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
)

const testSource = "source:sha256:0123456789abcdef"

func newTestStore(t *testing.T, directory string) *Store {
	t.Helper()
	return Open(Settings{Dir: directory, Enabled: true})
}

func listingCache(t *testing.T, store *Store) sessionio.ListingCache {
	t.Helper()
	cache, found := store.ListingCache(testSource)
	if !found {
		t.Fatal("an enabled store handed out no listing cache")
	}
	return cache
}

func testRef(id string, title string) sessionio.SessionRef {
	started := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	return sessionio.SessionRef{
		ID:                sessionio.SessionID(id),
		NativeID:          "native-" + id,
		Title:             title,
		DiscoveryRevision: sessionio.DiscoveryRevision("discovery-" + id),
		Native: sessionio.NativeSessionMetadata{
			Identities: []sessionio.NativeIdentity{{
				Kind:  sessionio.NativeIdentityKindSession,
				Value: "native-" + id,
			}},
		},
		Occurrence: sessionio.SourceOccurrence{
			ID:       sessionio.OccurrenceID("occurrence-" + id),
			SourceID: sessionio.SourceID(testSource),
			Harness:  sessionio.HarnessClaude,
		},
		StartedAt: &started,
		Diagnostics: []sessionio.Diagnostic{{
			Code:     "claude_invalid_timestamp",
			Severity: sessionio.DiagnosticSeverityWarning,
			Message:  "invalid Claude timestamp",
		}},
	}
}

func cacheFile(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read cache directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache directory holds %d files, want 1", len(entries))
	}
	return filepath.Join(directory, entries[0].Name())
}

func TestARetainedRecordSurvivesAFlushByteForByte(t *testing.T) {
	directory := t.TempDir()
	store := newTestStore(t, directory)
	retained := testRef("session-a", "first record")
	listingCache(t, store).Retain("occurrence-a", "stamp-1", retained)
	store.Flush()

	reopened := newTestStore(t, directory)
	loaded, found := listingCache(t, reopened).Lookup("occurrence-a", "stamp-1")
	if !found {
		t.Fatal("a retained record was not reused")
	}
	before, err := json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("reused record differs\nbefore: %s\nafter:  %s", before, after)
	}
	if diagnostics := reopened.Diagnostics(); len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}

func TestAChangedStampIsAMiss(t *testing.T) {
	directory := t.TempDir()
	store := newTestStore(t, directory)
	cache := listingCache(t, store)
	cache.Retain("occurrence-a", "stamp-1", testRef("session-a", "first"))
	if _, found := cache.Lookup("occurrence-a", "stamp-2"); found {
		t.Fatal("a changed stamp was reused")
	}
}

// retainTwo writes and flushes two listing records into one cache file.
func retainTwo(t *testing.T, directory string) {
	t.Helper()
	store := newTestStore(t, directory)
	cache := listingCache(t, store)
	cache.Retain("occurrence-a", "stamp-1", testRef("session-a", "first"))
	cache.Retain("occurrence-b", "stamp-2", testRef("session-b", "second"))
	store.Flush()
}

// requireDiscarded proves that no record of an unusable file was reused and
// that the discard was reported.
func requireDiscarded(t *testing.T, directory string) {
	t.Helper()
	store := newTestStore(t, directory)
	if _, found := listingCache(t, store).Lookup("occurrence-a", "stamp-1"); found {
		t.Fatal("a record of an unusable cache file was reused")
	}
	diagnostics := store.Diagnostics()
	if len(diagnostics) != 1 ||
		!strings.Contains(diagnostics[0].String(), "reader cache: discarded") {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}

// A cache file whose records do not carry the current schema is discarded
// whole: one unusable record never yields a partly reused listing.
func TestAnUnknownSchemaDiscardsTheWholeFile(t *testing.T) {
	directory := t.TempDir()
	retainTwo(t, directory)
	path := cacheFile(t, directory)
	mustWrite(t, path, strings.ReplaceAll(
		string(mustRead(t, path)),
		Schema,
		"sessionio.readercache/v0",
	))
	requireDiscarded(t, directory)
}

func TestATornRecordDiscardsTheWholeFile(t *testing.T) {
	directory := t.TempDir()
	retainTwo(t, directory)
	path := cacheFile(t, directory)
	lines := strings.Split(strings.TrimRight(string(mustRead(t, path)), "\n"), "\n")
	last := lines[len(lines)-1]
	lines[len(lines)-1] = last[:len(last)/2]
	mustWrite(t, path, strings.Join(lines, "\n")+"\n")
	requireDiscarded(t, directory)
}

// A rewritten file holds exactly what this run listed, so an occurrence that
// disappeared leaves no entry behind.
func TestAFlushDropsAnOccurrenceThisRunDidNotList(t *testing.T) {
	directory := t.TempDir()
	retainTwo(t, directory)

	second := newTestStore(t, directory)
	if _, found := listingCache(t, second).Lookup("occurrence-a", "stamp-1"); !found {
		t.Fatal("the first record was not reused")
	}
	second.Flush()

	third := newTestStore(t, directory)
	if _, found := listingCache(t, third).Lookup("occurrence-b", "stamp-2"); found {
		t.Fatal("an unlisted occurrence survived the rewrite")
	}
}

func TestAnUntouchedSourceIsNotRewritten(t *testing.T) {
	directory := t.TempDir()
	store := newTestStore(t, directory)
	listingCache(t, store).Retain("occurrence-a", "stamp-1", testRef("session-a", "x"))
	store.Flush()
	path := cacheFile(t, directory)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	idle := newTestStore(t, directory)
	if _, found := idle.ListingCache(testSource); !found {
		t.Fatal("an enabled store handed out no listing cache")
	}
	idle.Flush()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a source nobody listed was rewritten")
	}
}

func TestActivityNeedsTheSameDiscoveryRevision(t *testing.T) {
	directory := t.TempDir()
	store := newTestStore(t, directory)
	listingCache(t, store).Retain("occurrence-a", "stamp-1", testRef("a", "x"))
	activity := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	store.RetainActivity(testSource, "occurrence-a", "discovery-1", &activity)
	store.Flush()

	reopened := newTestStore(t, directory)
	if _, found := reopened.Activity(testSource, "occurrence-a", "discovery-2"); found {
		t.Fatal("activity of a superseded revision was reused")
	}
	retained, found := reopened.Activity(testSource, "occurrence-a", "discovery-1")
	if !found || retained == nil || !retained.Equal(activity) {
		t.Fatalf("activity = %v, %v", retained, found)
	}
}

// A session with no timestamped record resolves to no activity at all, which
// is an answer worth caching rather than repeating.
func TestResolvedAbsentActivityIsRetained(t *testing.T) {
	directory := t.TempDir()
	store := newTestStore(t, directory)
	listingCache(t, store).Retain("occurrence-a", "stamp-1", testRef("a", "x"))
	store.RetainActivity(testSource, "occurrence-a", "discovery-1", nil)
	store.Flush()

	reopened := newTestStore(t, directory)
	retained, found := reopened.Activity(testSource, "occurrence-a", "discovery-1")
	if !found || retained != nil {
		t.Fatalf("activity = %v, %v", retained, found)
	}
}

func TestADisabledStoreHandsOutNoCache(t *testing.T) {
	store := Open(Settings{Dir: t.TempDir(), Enabled: false})
	if _, found := store.ListingCache(testSource); found {
		t.Fatal("a disabled store handed out a cache")
	}
	if store.Enabled() {
		t.Fatal("a disabled store reports itself enabled")
	}
	store.Flush()
}

func TestAStoreWithoutADirectoryIsDisabled(t *testing.T) {
	store := Open(Settings{Enabled: true})
	if store.Enabled() {
		t.Fatal("a store without a directory reports itself enabled")
	}
}

// The cache file name must be usable on every platform: a source ID carries
// separators Windows rejects.
func TestTheCacheFileNameIsPortable(t *testing.T) {
	name := fileName("source:sha256:abc/def")
	if strings.ContainsAny(name, `:/\<>"|?*`) {
		t.Fatalf("cache file name = %q", name)
	}
	if name != "source-sha256-abc-def.ndjson" {
		t.Fatalf("cache file name = %q", name)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustWrite(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
