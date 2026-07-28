package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/readercache"
)

// cacheFixture is one Claude home listed through one advisory cache directory.
type cacheFixture struct {
	t         *testing.T
	home      string
	directory string
	primary   string
	agent     string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	fixture := &cacheFixture{
		t:         t,
		home:      t.TempDir(),
		directory: t.TempDir(),
		primary:   "11111111-1111-4111-8111-1111111111c1",
		agent:     "worker",
	}
	writeJSONL(t, fixture.transcript(), fixture.openingRecord(),
		`{"type":"custom-title","uuid":"c-2","sessionId":"`+fixture.primary+
			`","timestamp":"2026-07-28T09:00:01Z","customTitle":"AAAAA"}`,
	)
	writeJSONL(
		t,
		fixture.subagent(),
		`{"type":"assistant","uuid":"c-3","sessionId":"`+fixture.primary+
			`","agentId":"`+fixture.agent+
			`","timestamp":"2026-07-28T09:00:02Z","message":`+
			`{"role":"assistant","content":"subagent work"}}`,
	)
	writeFixtureFile(t, fixture.sidecar(), []byte(`{"agentType":"worker"}`))
	return fixture
}

func (fixture *cacheFixture) openingRecord() string {
	return `{"type":"user","uuid":"c-1","sessionId":"` + fixture.primary +
		`","timestamp":"2026-07-28T09:00:00Z","message":` +
		`{"role":"user","content":"cached listing"}}`
}

func (fixture *cacheFixture) transcript() string {
	return filepath.Join(
		fixture.home, "projects", "-cache", fixture.primary+".jsonl",
	)
}

func (fixture *cacheFixture) subagent() string {
	return filepath.Join(
		fixture.home, "projects", "-cache", fixture.primary,
		"subagents", "agent-"+fixture.agent+".jsonl",
	)
}

func (fixture *cacheFixture) sidecar() string {
	return strings.TrimSuffix(fixture.subagent(), ".jsonl") + ".meta.json"
}

// list opens a fresh store, lists, and flushes, exactly as one command run does.
func (fixture *cacheFixture) list() []sessionio.SessionRef {
	fixture.t.Helper()
	store := readercache.Open(readercache.Settings{
		Dir:     fixture.directory,
		Enabled: true,
	})
	adapter, err := New(Config{
		ConfigDir:      fixture.home,
		MaxRecordBytes: DefaultMaxRecordBytes,
		Cache:          store,
	})
	if err != nil {
		fixture.t.Fatal(err)
	}
	sessions := collectSessions(fixture.t, adapter)
	store.Flush()
	if diagnostics := store.Diagnostics(); len(diagnostics) != 0 {
		fixture.t.Fatalf("cache diagnostics = %v", diagnostics)
	}
	return sessions
}

func (fixture *cacheFixture) encoded() string {
	fixture.t.Helper()
	encoded, err := json.Marshal(fixture.list())
	if err != nil {
		fixture.t.Fatal(err)
	}
	return string(encoded)
}

// rewriteInPlace replaces bytes without changing the size or the modification
// time, so only a reader that reopened the transcript can see the change.
func (fixture *cacheFixture) rewriteInPlace(path string, from string, to string) {
	fixture.t.Helper()
	if len(from) != len(to) {
		fixture.t.Fatalf("in-place rewrite changes the size: %q -> %q", from, to)
	}
	info, err := os.Stat(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	body := string(mustReadFile(fixture.t, path))
	if !strings.Contains(body, from) {
		fixture.t.Fatalf("%s does not carry %q", path, from)
	}
	writeFixtureFile(fixture.t, path, []byte(strings.Replace(body, from, to, 1)))
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *cacheFixture) append(path string, record string) {
	fixture.t.Helper()
	body := string(mustReadFile(fixture.t, path)) + record + "\n"
	writeFixtureFile(fixture.t, path, []byte(body))
	fixture.advance(path)
}

// advance moves the modification time forward, because a rewrite inside one
// filesystem timestamp tick is otherwise indistinguishable.
func (fixture *cacheFixture) advance(path string) {
	fixture.t.Helper()
	moment := time.Now().Add(time.Second)
	if err := os.Chtimes(path, moment, moment); err != nil {
		fixture.t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A warm listing must reproduce every field of every record, so the retained
// entry is compared against a listing that read the transcripts.
func TestAWarmListingReproducesEveryListingRecord(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	warm := fixture.encoded()
	if cold != warm {
		t.Fatalf("warm listing differs\ncold: %s\nwarm: %s", cold, warm)
	}
	if !strings.Contains(cold, `"title":"AAAAA"`) {
		t.Fatalf("listing carries no title: %s", cold)
	}
}

// An unchanged stat identity is the whole reuse condition, so a transcript
// rewritten in place behind the cache's back must still list as it was.
func TestAnUnchangedStampReusesTheRetainedRecord(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	fixture.rewriteInPlace(fixture.transcript(), `"AAAAA"`, `"BBBBB"`)
	if warm := fixture.encoded(); warm != cold {
		t.Fatalf("an unchanged stamp was not reused\ncold: %s\nwarm: %s", cold, warm)
	}
}

func TestAnAppendedTranscriptIsListedAgain(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	fixture.append(
		fixture.transcript(),
		`{"type":"custom-title","uuid":"c-4","sessionId":"`+fixture.primary+
			`","timestamp":"2026-07-28T09:10:00Z","customTitle":"appended"}`,
	)
	warm := fixture.encoded()
	if warm == cold || !strings.Contains(warm, `"title":"appended"`) {
		t.Fatalf("an appended transcript was not listed again: %s", warm)
	}
}

func TestATruncatedTranscriptIsListedAgain(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	writeJSONL(t, fixture.transcript(), fixture.openingRecord())
	fixture.advance(fixture.transcript())
	warm := fixture.encoded()
	if warm == cold || strings.Contains(warm, `"title":"AAAAA"`) {
		t.Fatalf("a truncated transcript was not listed again: %s", warm)
	}
}

// The sidecar is part of the listing record, so editing it must invalidate the
// entry of the occurrence it belongs to and of no other.
func TestAnEditedSidecarIsListedAgain(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	writeFixtureFile(
		t,
		fixture.sidecar(),
		[]byte(`{"agentType":"reviewer","model":"m"}`),
	)
	fixture.advance(fixture.sidecar())
	warm := fixture.encoded()
	if warm == cold || !strings.Contains(warm, `"role":"reviewer"`) {
		t.Fatalf("an edited sidecar was not listed again: %s", warm)
	}
	if !strings.Contains(warm, `"title":"AAAAA"`) {
		t.Fatalf("the untouched occurrence was lost: %s", warm)
	}
}

func TestACreatedSidecarIsListedAgain(t *testing.T) {
	fixture := newCacheFixture(t)
	if err := os.Remove(fixture.sidecar()); err != nil {
		t.Fatal(err)
	}
	cold := fixture.encoded()
	writeFixtureFile(t, fixture.sidecar(), []byte(`{"agentType":"worker"}`))
	warm := fixture.encoded()
	if warm == cold || !strings.Contains(warm, `"role":"worker"`) {
		t.Fatalf("a created sidecar was not listed again: %s", warm)
	}
}

func TestADisabledCacheListsFromTheSource(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	store := readercache.Open(readercache.Settings{
		Dir:     fixture.directory,
		Enabled: false,
	})
	adapter, err := New(Config{
		ConfigDir:      fixture.home,
		MaxRecordBytes: DefaultMaxRecordBytes,
		Cache:          store,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.rewriteInPlace(fixture.transcript(), `"AAAAA"`, `"BBBBB"`)
	sessions := collectSessions(t, adapter)
	encoded, err := json.Marshal(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == cold || !strings.Contains(string(encoded), `"BBBBB"`) {
		t.Fatalf("a disabled cache served a retained record: %s", encoded)
	}
}

func TestANativeKeyIsTheHarnessRecordIdentifier(t *testing.T) {
	fixture := newCacheFixture(t)
	adapter := newTestAdapter(t, fixture.home)
	sessions := collectSessions(t, adapter)
	items := collectItems(t, adapter, sessionByNativeID(t, sessions, fixture.primary))
	if len(items) == 0 {
		t.Fatal("the primary transcript produced no item")
	}
	if items[0].Observation.NativeKey != "c-1" {
		t.Fatalf("native key = %q", items[0].Observation.NativeKey)
	}
	// A sidecar record is not a native transcript record and has no key.
	agent := sessionByNativeID(t, sessions, fixture.agent)
	for _, item := range collectItems(t, adapter, agent) {
		if item.Observation.NativeKind == "agent_metadata" &&
			item.Observation.NativeKey != "" {
			t.Fatalf("sidecar native key = %q", item.Observation.NativeKey)
		}
	}
	if err := context.Background().Err(); err != nil {
		t.Fatal(err)
	}
}
