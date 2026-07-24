package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	if config.Home != "" || config.MaxRecordBytes != DefaultMaxRecordBytes {
		t.Fatalf("DefaultConfig() = %#v", config)
	}
	if DefaultMaxRecordBytes != 64<<20 {
		t.Fatalf("DefaultMaxRecordBytes = %d", DefaultMaxRecordBytes)
	}
}

func TestStableIDAndDiscoveryRevisionInputs(t *testing.T) {
	const expected = "fixture:sha256:67b10d69410338934db2cb43c048f2a33ad51d2bb7a6cdbfaecd968eceaa374e"
	if actual := derivedID("fixture", "alpha", "beta"); actual != expected {
		t.Fatalf("derived ID = %q, want %q", actual, expected)
	}
	home := t.TempDir()
	adapter := newFixtureAdapter(t, home)
	if adapter.sourceID != sessionio.SourceID(derivedID("source", string(sessionio.HarnessCodex), home)) {
		t.Fatalf("source ID = %q", adapter.sourceID)
	}
	sampleOccurrence := occurrence{relative: "archived_sessions/rollout-2026-07-24T00-00-00-10000000-0000-4000-8000-000000000000.jsonl"}
	base := adapter.discoveryRevisionAt(sampleOccurrence, []byte("header\n"), 10, 20)
	for name, changed := range map[string]sessionio.DiscoveryRevision{
		"framing": adapter.discoveryRevisionAt(sampleOccurrence, []byte("header\r\n"), 10, 20),
		"size":    adapter.discoveryRevisionAt(sampleOccurrence, []byte("header\n"), 11, 20),
		"mtime":   adapter.discoveryRevisionAt(sampleOccurrence, []byte("header\n"), 10, 21),
		"path":    adapter.discoveryRevisionAt(occurrence{relative: sampleOccurrence.relative + ".copy"}, []byte("header\n"), 10, 20),
	} {
		if changed == base {
			t.Fatalf("%s did not change discovery revision", name)
		}
	}
}

func TestNewRejectsInvalidLimit(t *testing.T) {
	for _, limit := range []int64{0, -2} {
		if _, err := New(Config{Home: t.TempDir(), MaxRecordBytes: limit}); err == nil {
			t.Fatalf("New(%d) error = nil", limit)
		}
	}
	if _, err := New(Config{Home: t.TempDir(), MaxRecordBytes: sourceio.UnlimitedRecordBytes}); err != nil {
		t.Fatalf("New(unlimited) error = %v", err)
	}
}

func TestDiscoveryMissingRootsAndSourceFilter(t *testing.T) {
	adapter := newFixtureAdapter(t, t.TempDir())
	source := nextSource(t, adapter)
	if source.Status != sessionio.SourceStatusMissing || len(source.Diagnostics) != 2 {
		t.Fatalf("missing source = %#v", source)
	}
	home := fixtureHome(t)
	adapter = newFixtureAdapter(t, home)
	stream, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{Sources: []sessionio.SourceID{"other", adapter.sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("mixed source filter = %v", err)
	}
}

func TestPlainDiscoveryAndRead(t *testing.T) {
	home := fixtureHome(t)
	adapter := newFixtureAdapter(t, home)
	source := nextSource(t, adapter)
	if source.Status != sessionio.SourceStatusAvailable || source.Harness != sessionio.HarnessCodex {
		t.Fatalf("source = %#v", source)
	}
	if len(source.Capabilities) != 10 || source.Capabilities[0].Support != sessionio.SupportPartial {
		t.Fatalf("capabilities = %#v", source.Capabilities)
	}
	sessions := collectSessions(t, adapter)
	if len(sessions) != 17 {
		t.Fatalf("sessions = %d, want 17", len(sessions))
	}
	for _, session := range sessions {
		if session.DiscoveryRevision == "" || session.Occurrence.Locator.File == nil {
			t.Fatalf("invalid discovered session %#v", session)
		}
	}
	current := sessionByIdentity(t, sessions, "session-current")
	if current.ID == "" {
		t.Fatal("current session missing")
	}
	if got := current.Native.Identities; len(got) != 1 || got[0].Value != "session-current" {
		t.Fatalf("identities = %#v", got)
	}
	if current.Native.History == nil || current.Native.History.OwnStartOrdinal == nil || *current.Native.History.OwnStartOrdinal != 0 {
		t.Fatalf("history = %#v", current.Native.History)
	}
	items := collectReadItems(t, adapter, current)
	if len(items) != 5 {
		t.Fatalf("read items = %d, want 5", len(items))
	}
	if items[0].Observation.Representation.Capture != sessionio.CaptureKindByteExact || items[0].Observation.Revision.Value == "" {
		t.Fatalf("observation = %#v", items[0].Observation)
	}
	if items[0].Events[0].Facts == nil || items[0].Events[0].Facts.Facts[0].Value != "/work/current" {
		t.Fatalf("metadata facts = %#v", items[0].Events)
	}
	if items[1].Events[0].Kind != sessionio.EventKindMessage || items[2].Events[0].Kind != sessionio.EventKindToolCall || items[3].Events[0].Kind != sessionio.EventKindToolResult {
		t.Fatalf("events = %#v %#v %#v", items[1].Events, items[2].Events, items[3].Events)
	}
	if len(items[3].Relations) != 1 || items[3].Relations[0].Kind != sessionio.RelationKindToolPair {
		t.Fatalf("tool pair = %#v", items[3].Relations)
	}
	if items[4].Events[0].Kind != sessionio.EventKindUsage {
		t.Fatalf("usage event = %#v", items[4].Events)
	}
	if items[0].Session.DiscoveryRevision == "" || items[0].Session.DiscoveryRevision != items[4].Session.DiscoveryRevision {
		t.Fatalf("read discovery revisions differ")
	}
}

func TestDiscoveryOrderingAndRawReconstruction(t *testing.T) {
	home := fixtureHome(t)
	adapter := newFixtureAdapter(t, home)
	sessions := collectSessions(t, adapter)
	expectedPaths := []string{
		"archived_sessions/rollout-2026-07-21T10-00-00-10000000-0000-4000-8000-000000000001.jsonl",
		"archived_sessions/rollout-2026-07-21T10-00-02-10000000-0000-4000-8000-000000000003.jsonl",
		"archived_sessions/rollout-2026-07-21T10-00-03-10000000-0000-4000-8000-000000000004.jsonl",
		"archived_sessions/rollout-2026-07-21T10-00-04-10000000-0000-4000-8000-000000000005.jsonl",
		"archived_sessions/rollout-2026-07-22T10-00-00-10000000-0000-4000-8000-000000000006.jsonl",
		"archived_sessions/rollout-2026-07-23T10-00-00-10000000-0000-4000-8000-000000000008.jsonl",
		"sessions/2026/07/21/rollout-2026-07-21T10-00-00-10000000-0000-4000-8000-000000000001.jsonl",
		"sessions/2026/07/22/rollout-2026-07-22T10-00-00-10000000-0000-4000-8000-000000000006.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000010.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T11-00-00-10000000-0000-4000-8000-000000000011.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T12-00-00-10000000-0000-4000-8000-000000000012.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T13-00-00-10000000-0000-4000-8000-000000000013.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T14-00-00-10000000-0000-4000-8000-000000000014.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T15-00-00-10000000-0000-4000-8000-000000000015.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T16-00-00-10000000-0000-4000-8000-000000000016.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T17-00-00-10000000-0000-4000-8000-000000000017.jsonl",
		"sessions/2026/07/24/rollout-2026-07-24T18-00-00-10000000-0000-4000-8000-000000000018.jsonl",
	}
	if len(sessions) != len(expectedPaths) {
		t.Fatalf("sessions = %d, want %d", len(sessions), len(expectedPaths))
	}
	for index, session := range sessions {
		path := session.Occurrence.Locator.File.Path
		if path != expectedPaths[index] {
			t.Fatalf("session path %d = %q, want %q", index, path, expectedPaths[index])
		}
		if hasIdentity(session, "session-malformed") || hasIdentity(session, "session-known-malformed") {
			continue
		}
		items := collectReadItems(t, adapter, session)
		var reconstructed []byte
		var previousEnd int64
		for itemIndex, item := range items {
			locator := item.Observation.Locator.File
			if locator == nil || locator.Record == nil || *locator.Record != uint64(itemIndex+1) ||
				locator.Line == nil || *locator.Line != uint64(itemIndex+1) ||
				locator.ByteRange == nil || locator.ByteRange.Start != previousEnd {
				t.Fatalf("%s locator %d = %#v", path, itemIndex, locator)
			}
			reconstructed = append(reconstructed, item.Observation.Representation.Data...)
			reconstructed = append(reconstructed, item.Observation.Representation.Framing...)
			previousEnd = locator.ByteRange.End + int64(len(item.Observation.Representation.Framing))
		}
		native, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if len(reconstructed) > len(native) || !bytes.Equal(reconstructed, native[:len(reconstructed)]) {
			t.Fatalf("%s reconstruction differs", path)
		}
		if !hasIdentity(session, "session-pending") && len(reconstructed) != len(native) {
			t.Fatalf("%s reconstruction length = %d, want %d", path, len(reconstructed), len(native))
		}
	}
}

func TestActivePendingTailAndMalformedInterior(t *testing.T) {
	adapter := newFixtureAdapter(t, fixtureHome(t))
	sessions := collectSessions(t, adapter)
	for _, session := range sessions {
		switch {
		case hasIdentity(session, "session-pending"):
			items := collectReadItems(t, adapter, session)
			if len(items) != 1 {
				t.Fatalf("pending items = %d, want 1", len(items))
			}
		case hasIdentity(session, "session-malformed"):
			_, err := adapter.Read(context.Background(), session)
			if err == nil || !strings.Contains(err.Error(), "record=2") {
				t.Fatalf("malformed error = %v", err)
			}
			readerError := assertReaderError(t, err, "read")
			if readerError.SessionID != session.ID || readerError.Locator == nil ||
				readerError.Locator.File == nil || readerError.Locator.File.Record == nil ||
				*readerError.Locator.File.Record != 2 {
				t.Fatalf("malformed reader error = %#v", readerError)
			}
		}
	}
}

func TestPendingFirstHeaderAndFinalUnterminatedRecord(t *testing.T) {
	home := t.TempDir()
	writeRollout(
		t,
		home,
		true,
		"rollout-2026-07-24T09-00-00-10000000-0000-4000-8000-000000000090.jsonl",
		nil,
	)
	pending := []byte(`{"id":"10000000-0000-4000-8000-000000000091","type":"session_meta"}`)
	writeRollout(
		t,
		home,
		true,
		"rollout-2026-07-24T09-00-01-10000000-0000-4000-8000-000000000091.jsonl",
		pending,
	)
	final := []byte(`{"id":"10000000-0000-4000-8000-000000000092","type":"session_meta"}`)
	writeRollout(
		t,
		home,
		false,
		"rollout-2026-07-24T09-00-02-10000000-0000-4000-8000-000000000092.jsonl",
		final,
	)
	adapter := newFixtureAdapter(t, home)
	sessions := collectSessions(t, adapter)
	if len(sessions) != 1 || sessions[0].NativeID != "10000000-0000-4000-8000-000000000092" {
		t.Fatalf("sessions = %#v", sessions)
	}
	items := collectReadItems(t, adapter, sessions[0])
	if len(items) != 1 || len(items[0].Observation.Representation.Framing) != 0 ||
		!bytes.Equal(items[0].Observation.Representation.Data, final) {
		t.Fatalf("unterminated final item = %#v", items)
	}
}

func TestRecordLimitAndUnknown(t *testing.T) {
	home := fixtureHome(t)
	adapter, err := New(Config{Home: home, MaxRecordBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{}); err == nil || !strings.Contains(err.Error(), "limit=10") {
		t.Fatalf("limit error = %v", err)
	}
	adapter = newFixtureAdapter(t, home)
	for _, session := range collectSessions(t, adapter) {
		if !hasIdentity(session, "session-unknown") {
			continue
		}
		items := collectReadItems(t, adapter, session)
		if items[1].Events[0].Kind != sessionio.EventKindUnknown || len(items[1].Diagnostics) != 2 ||
			!hasDiagnostic(items[1].Diagnostics, "codex_invalid_timestamp") ||
			!hasDiagnostic(items[1].Diagnostics, "codex_unknown_record_kind") {
			t.Fatalf("unknown item = %#v", items[1])
		}
	}
}

func TestRecordLimitBoundaries(t *testing.T) {
	const limit = 256
	const rolloutName = "rollout-2026-07-24T20-00-00-10000000-0000-4000-8000-000000000020.jsonl"
	nativeID := "10000000-0000-4000-8000-000000000020"
	for _, testCase := range []struct {
		name      string
		size      int
		active    bool
		framing   string
		wantError bool
		maxBytes  int64
	}{
		{name: "below", size: limit - 1, framing: "\n", maxBytes: limit},
		{name: "equal_crlf", size: limit, framing: "\r\n", maxBytes: limit},
		{name: "above", size: limit + 1, framing: "\n", wantError: true, maxBytes: limit},
		{name: "active_pending_above", size: limit + 1, active: true, wantError: true, maxBytes: limit},
		{name: "unlimited", size: limit + 1, framing: "\n", maxBytes: sourceio.UnlimitedRecordBytes},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			record := paddedMetadataRecord(t, testCase.size, nativeID)
			writeRollout(t, home, testCase.active, rolloutName, append(record, testCase.framing...))
			adapter, err := New(Config{Home: home, MaxRecordBytes: testCase.maxBytes})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{})
			if testCase.wantError {
				if err == nil {
					stream.Close()
					t.Fatal("Sessions() error = nil")
				}
				readerError := assertReaderError(t, err, "sessions")
				if readerError.Locator == nil || readerError.Locator.File == nil ||
					readerError.Locator.File.Record == nil || *readerError.Locator.File.Record != 1 ||
					readerError.Locator.File.Line == nil || *readerError.Locator.File.Line != 1 ||
					!strings.Contains(err.Error(), "limit=256") ||
					!strings.Contains(err.Error(), "observed-at-least=257") {
					t.Fatalf("limit error = %#v: %v", readerError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			session, err := stream.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if session.NativeID != nativeID {
				t.Fatalf("session = %#v", session)
			}
		})
	}
}

func TestReadLimitErrorIncludesSessionAndLocator(t *testing.T) {
	const limit = 256
	home := t.TempDir()
	name := "rollout-2026-07-24T21-00-00-10000000-0000-4000-8000-000000000021.jsonl"
	header := []byte(`{"id":"10000000-0000-4000-8000-000000000021","type":"session_meta"}`)
	oversized := paddedUnknownRecord(t, limit+1)
	data := append(append(append([]byte(nil), header...), '\n'), oversized...)
	data = append(data, '\n')
	writeRollout(t, home, false, name, data)
	adapter, err := New(Config{Home: home, MaxRecordBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	session := collectSessions(t, adapter)[0]
	_, err = adapter.Read(context.Background(), session)
	readerError := assertReaderError(t, err, "read")
	if readerError.SessionID != session.ID || readerError.Locator == nil ||
		readerError.Locator.File == nil || readerError.Locator.File.Record == nil ||
		*readerError.Locator.File.Record != 2 || readerError.Locator.File.Line == nil ||
		*readerError.Locator.File.Line != 2 {
		t.Fatalf("read limit error = %#v", readerError)
	}
}

func TestInvalidCanonicalTimestampIsDiagnostic(t *testing.T) {
	home := t.TempDir()
	name := "rollout-2026-07-24T22-00-00-10000000-0000-4000-8000-000000000022.jsonl"
	data := []byte(`{"timestamp":"not-a-time","type":"session_meta","payload":{"id":"10000000-0000-4000-8000-000000000022","session_id":"session-invalid-time"}}` + "\n")
	writeRollout(t, home, false, name, data)
	adapter := newFixtureAdapter(t, home)
	session := sessionByIdentity(t, collectSessions(t, adapter), "session-invalid-time")
	if session.StartedAt != nil || len(session.Diagnostics) != 1 ||
		session.Diagnostics[0].Code != "codex_invalid_timestamp" ||
		session.Diagnostics[0].Cause == nil || session.Diagnostics[0].Locator == nil ||
		session.Diagnostics[0].Locator.File == nil ||
		session.Diagnostics[0].Locator.File.Record == nil {
		t.Fatalf("invalid timestamp session = %#v", session)
	}
	items := collectReadItems(t, adapter, session)
	if len(items) != 1 || items[0].Observation.Timestamp != nil ||
		len(items[0].Diagnostics) != 1 ||
		items[0].Diagnostics[0].Code != "codex_invalid_timestamp" ||
		items[0].Diagnostics[0].Cause == nil {
		t.Fatalf("invalid timestamp item = %#v", items)
	}
}

func TestStaleSessionReadUsesFreshGeneration(t *testing.T) {
	home := fixtureHome(t)
	adapter := newFixtureAdapter(t, home)
	stale := sessionByIdentity(t, collectSessions(t, adapter), "session-current")
	before := collectReadItems(t, adapter, stale)
	fileName := filepath.Join(home, filepath.FromSlash(stale.Occurrence.Locator.File.Path))
	file, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	addition := []byte(`{"timestamp":"2026-07-24T10:00:05Z","type":"future_after_append","payload":{"value":1}}` + "\n")
	if _, err := file.Write(addition); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	after := collectReadItems(t, adapter, stale)
	if len(after) != len(before)+1 {
		t.Fatalf("after items = %d, want %d", len(after), len(before)+1)
	}
	fresh := sessionByIdentity(t, collectSessions(t, adapter), "session-current")
	if fresh.ID != stale.ID || fresh.DiscoveryRevision == stale.DiscoveryRevision ||
		after[0].Session.DiscoveryRevision != fresh.DiscoveryRevision {
		t.Fatalf("revisions stale=%q read=%q fresh=%q", stale.DiscoveryRevision, after[0].Session.DiscoveryRevision, fresh.DiscoveryRevision)
	}
	for index := range before {
		if after[index].Observation.ID != before[index].Observation.ID {
			t.Fatalf("observation %d changed: %q != %q", index, after[index].Observation.ID, before[index].Observation.ID)
		}
		if after[index].Session.DiscoveryRevision != after[0].Session.DiscoveryRevision ||
			after[index].Observation.Revision != after[0].Observation.Revision {
			t.Fatalf("read generation diverged at %d", index)
		}
	}
	if after[0].Observation.Revision == before[0].Observation.Revision {
		t.Fatal("generation revision did not change after append")
	}
}

func TestToolPairResultBeforeCall(t *testing.T) {
	home := t.TempDir()
	name := "rollout-2026-07-24T23-00-00-10000000-0000-4000-8000-000000000023.jsonl"
	data := []byte(
		`{"id":"10000000-0000-4000-8000-000000000023","type":"session_meta"}` + "\n" +
			`{"type":"function_call_output","call_id":"reverse","output":"done"}` + "\n" +
			`{"type":"function_call","call_id":"reverse","name":"shell","arguments":"{}"}` + "\n",
	)
	writeRollout(t, home, false, name, data)
	adapter := newFixtureAdapter(t, home)
	items := collectReadItems(t, adapter, collectSessions(t, adapter)[0])
	if len(items) != 3 || len(items[1].Relations) != 0 || len(items[2].Relations) != 1 {
		t.Fatalf("reverse tool items = %#v", items)
	}
	relation := items[2].Relations[0]
	call := items[2].Events[0]
	result := items[1].Events[0]
	if relation.Kind != sessionio.RelationKindToolPair ||
		relation.From.ID != string(call.ID) || relation.To.ID != string(result.ID) ||
		len(relation.Evidence) != 2 ||
		relation.Evidence[0].Observation != items[2].Observation.ID ||
		relation.Evidence[1].Observation != items[1].Observation.ID {
		t.Fatalf("reverse tool relation = %#v", relation)
	}
}

func TestOperationalToolPair(t *testing.T) {
	home := t.TempDir()
	name := "rollout-2026-07-24T23-00-01-10000000-0000-4000-8000-000000000024.jsonl"
	data := []byte(
		`{"id":"10000000-0000-4000-8000-000000000024","type":"session_meta"}` + "\n" +
			`{"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"operational","command":["pwd"],"cwd":"/work"}}` + "\n" +
			`{"type":"event_msg","payload":{"type":"exec_command_end","call_id":"operational","stdout":"/work\n","stderr":"","exit_code":0}}` + "\n",
	)
	writeRollout(t, home, false, name, data)
	adapter := newFixtureAdapter(t, home)
	items := collectReadItems(t, adapter, collectSessions(t, adapter)[0])
	call := items[1].Events[0].ToolCall
	result := items[2].Events[0].ToolResult
	if call == nil || call.Name != "exec_command" || call.Input.MediaType != "application/json" ||
		result == nil || result.Status != sessionio.ToolResultStatusSuccess ||
		len(items[2].Relations) != 1 || items[2].Relations[0].Kind != sessionio.RelationKindToolPair {
		t.Fatalf("operational pair = %#v %#v relations=%#v", items[1].Events, items[2].Events, items[2].Relations)
	}
}

func TestToolPairDoesNotCrossOccurrences(t *testing.T) {
	home := t.TempDir()
	writeRollout(
		t,
		home,
		false,
		"rollout-2026-07-24T23-00-04-10000000-0000-4000-8000-000000000030.jsonl",
		[]byte(
			`{"id":"10000000-0000-4000-8000-000000000030","type":"session_meta"}`+"\n"+
				`{"type":"function_call","call_id":"cross","name":"shell","arguments":"{}"}`+"\n",
		),
	)
	writeRollout(
		t,
		home,
		false,
		"rollout-2026-07-24T23-00-05-10000000-0000-4000-8000-000000000031.jsonl",
		[]byte(
			`{"id":"10000000-0000-4000-8000-000000000031","type":"session_meta"}`+"\n"+
				`{"type":"function_call_output","call_id":"cross","output":"done"}`+"\n",
		),
	)
	adapter := newFixtureAdapter(t, home)
	for _, session := range collectSessions(t, adapter) {
		for _, item := range collectReadItems(t, adapter, session) {
			if len(item.Relations) != 0 {
				t.Fatalf("cross-occurrence relation = %#v", item.Relations)
			}
		}
	}
}

func TestKnownMalformedProjectionFailsWithLocator(t *testing.T) {
	tests := []struct {
		name     string
		record   string
		expected string
	}{
		{
			name:     "empty token count",
			record:   `{"type":"event_msg","payload":{"type":"token_count","info":null}}`,
			expected: "has no supported counters",
		},
		{
			name:     "invalid inline media",
			record:   `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_audio","audio_url":"data:audio/wav;base64,%%%"}]}}`,
			expected: "decode base64 data URI",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			name := "rollout-2026-07-24T23-00-02-10000000-0000-4000-8000-000000000025.jsonl"
			data := []byte(
				`{"id":"10000000-0000-4000-8000-000000000025","type":"session_meta"}` + "\n" +
					testCase.record + "\n",
			)
			writeRollout(t, home, false, name, data)
			adapter := newFixtureAdapter(t, home)
			session := collectSessions(t, adapter)[0]
			stream, err := adapter.Read(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if _, err := stream.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Next(context.Background())
			readerError := assertReaderError(t, err, "read")
			if readerError.SessionID != session.ID || readerError.Locator == nil ||
				readerError.Locator.File == nil || readerError.Locator.File.Record == nil ||
				*readerError.Locator.File.Record != 2 || !strings.Contains(err.Error(), testCase.expected) {
				t.Fatalf("known malformed error = %#v: %v", readerError, err)
			}
		})
	}
}

func TestSessionHeaderOpenErrorIncludesLocator(t *testing.T) {
	adapter := newFixtureAdapter(t, t.TempDir())
	occurrence := occurrence{
		relative: "archived_sessions/rollout-2026-07-24T23-00-03-10000000-0000-4000-8000-000000000026.jsonl",
	}
	_, err := adapter.readSessionRef(context.Background(), occurrence)
	readerError := assertReaderError(t, err, "sessions")
	if readerError.Locator == nil || readerError.Locator.File == nil ||
		readerError.Locator.File.Root != adapter.home ||
		readerError.Locator.File.Path != occurrence.relative {
		t.Fatalf("header open error = %#v", readerError)
	}
}

func TestCopiedAndParallelEvidenceStaysDistinct(t *testing.T) {
	adapter := newFixtureAdapter(t, fixtureHome(t))
	sessions := collectSessions(t, adapter)
	var copies []sessionio.SessionRef
	var parallel, compaction, malformed, child, uniqueChild sessionio.SessionRef
	for _, session := range sessions {
		switch {
		case session.NativeID == "10000000-0000-4000-8000-000000000006":
			copies = append(copies, session)
		case hasIdentity(session, "session-parallel"):
			parallel = session
		case hasIdentity(session, "session-compaction"):
			compaction = session
		case hasIdentity(session, "session-known-malformed"):
			malformed = session
		case session.NativeID == "10000000-0000-4000-8000-000000000003":
			child = session
		case session.NativeID == "10000000-0000-4000-8000-000000000005":
			uniqueChild = session
		}
	}
	if len(copies) != 2 || copies[0].ID == copies[1].ID || copies[0].Occurrence.ID == copies[1].Occurrence.ID {
		t.Fatalf("copies = %#v", copies)
	}
	if len(child.Native.Relationships) != 1 || child.Native.Relationships[0].Kind != sessionio.NativeRelationshipKindForkParent {
		t.Fatalf("child metadata = %#v", child.Native)
	}
	branchItems := collectReadItems(t, adapter, child)
	if len(branchItems[0].Relations) != 0 {
		t.Fatalf("ambiguous branch relations = %#v", branchItems[0].Relations)
	}
	branchItems = collectReadItems(t, adapter, uniqueChild)
	if len(branchItems[0].Relations) != 1 || branchItems[0].Relations[0].Kind != sessionio.RelationKindBranchParent {
		t.Fatalf("unique branch relations = %#v", branchItems[0].Relations)
	}
	items := collectReadItems(t, adapter, parallel)
	if len(items) != 5 || items[1].Observation.ID == items[2].Observation.ID || len(items[1].Relations) != 0 || len(items[2].Relations) != 0 || len(items[3].Relations) != 0 {
		t.Fatalf("parallel evidence = %#v", items)
	}
	items = collectReadItems(t, adapter, compaction)
	if items[1].Events[0].Kind != sessionio.EventKindMarker {
		t.Fatalf("compaction event = %#v", items[1].Events)
	}
	stream, err := adapter.Read(context.Background(), malformed)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "response_item payload type is required") {
		t.Fatalf("known shape error = %v", err)
	}
}

func TestCompressedOnlyIsReportedButNotListed(t *testing.T) {
	home := fixtureHome(t)
	compressedOnly := "rollout-2026-07-22T10-00-02-10000000-0000-4000-8000-000000000020.jsonl.zst"
	path := filepath.Join(home, "archived_sessions", compressedOnly)
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := "rollout-2026-07-22T10-00-00-10000000-0000-4000-8000-000000000006.jsonl.zst"
	if err := os.WriteFile(filepath.Join(home, "archived_sessions", sibling), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "archived_sessions", "rollout-invalid.jsonl.zst"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := newFixtureAdapter(t, home)
	source := nextSource(t, adapter)
	skipped := ""
	for _, diagnostic := range source.Diagnostics {
		if diagnostic.Code == "codex_compressed_skipped" {
			skipped = diagnostic.Message
		}
	}
	if !strings.Contains(skipped, "1 compressed rollout occurrence") {
		t.Fatalf("diagnostics = %#v", source.Diagnostics)
	}
	for _, session := range collectSessions(t, adapter) {
		if strings.HasSuffix(session.Occurrence.Locator.File.Path, ".zst") {
			t.Fatal("compressed occurrence listed")
		}
	}
}

func TestRolloutFilenameGrammar(t *testing.T) {
	valid := []string{
		"rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000010.jsonl",
		"rollout-2024-02-29T23-59-59-ABCDEF00-0000-4000-8000-000000000010.jsonl",
	}
	for _, name := range valid {
		if !plainName(name) {
			t.Errorf("plainName(%q) = false", name)
		}
	}
	invalid := []string{
		"rollout-2026-07-24T10-00-00-current.jsonl",
		"rollout-2026-02-30T10-00-00-10000000-0000-4000-8000-000000000010.jsonl",
		"rollout-2026-07-24T24-00-00-10000000-0000-4000-8000-000000000010.jsonl",
		"rollout-2026-07-24T10-00-00-100000000000-4000-8000-000000000010.jsonl",
		"prefix-rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000010.jsonl",
		"rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000010.jsonl.tmp",
	}
	for _, name := range invalid {
		if plainName(name) {
			t.Errorf("plainName(%q) = true", name)
		}
	}
}

func TestDiscoveryReportsSymlinkAndFilenameMismatch(t *testing.T) {
	home := fixtureHome(t)
	target := filepath.Join(
		home,
		"sessions",
		"2026",
		"07",
		"24",
		"rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000010.jsonl",
	)
	linkName := "rollout-2026-07-24T19-00-00-10000000-0000-4000-8000-000000000019.jsonl"
	link := filepath.Join(home, "sessions", "2026", "07", "24", linkName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	adapter := newFixtureAdapter(t, home)
	source := nextSource(t, adapter)
	foundSymlink := false
	for _, diagnostic := range source.Diagnostics {
		if diagnostic.Code == "codex_symlink_skipped" {
			foundSymlink = diagnostic.Locator != nil &&
				diagnostic.Locator.File != nil &&
				strings.HasSuffix(diagnostic.Locator.File.Path, linkName)
		}
	}
	if !foundSymlink {
		t.Fatalf("symlink diagnostics = %#v", source.Diagnostics)
	}
	if sessions := collectSessions(t, adapter); len(sessions) != 17 {
		t.Fatalf("sessions with symlink = %d, want 17", len(sessions))
	}

	sessions := collectSessions(t, adapter)
	mismatch := sessionByIdentity(t, sessions, "session-metadata-wins")
	foundMismatch := false
	mismatchCount := 0
	for _, session := range sessions {
		for _, diagnostic := range session.Diagnostics {
			if diagnostic.Code == "codex_filename_metadata_mismatch" {
				mismatchCount++
				if session.ID == mismatch.ID {
					foundMismatch = strings.Contains(diagnostic.Message, "10000000-0000-4000-8000-000000000013") &&
						strings.Contains(diagnostic.Message, "20000000-0000-4000-8000-000000000013")
				}
			}
		}
	}
	if !foundMismatch || mismatchCount != 1 {
		t.Fatalf("mismatch diagnostics = %#v", mismatch.Diagnostics)
	}
}

func TestConfiguredHomeSymlinkIsFollowedLiterally(t *testing.T) {
	target := fixtureHome(t)
	parent := t.TempDir()
	home := filepath.Join(parent, "codex-home-link")
	if err := os.Symlink(target, home); err != nil {
		t.Fatal(err)
	}
	adapter := newFixtureAdapter(t, home)
	source := nextSource(t, adapter)
	if source.Status != sessionio.SourceStatusAvailable || source.Locator.File == nil ||
		source.Locator.File.Root != home || len(collectSessions(t, adapter)) != 17 {
		t.Fatalf("symlinked home source = %#v", source)
	}
}

func TestCurrentRichNormalization(t *testing.T) {
	adapter := newFixtureAdapter(t, fixtureHome(t))
	session := sessionByIdentity(t, collectSessions(t, adapter), "session-rich")
	items := collectReadItems(t, adapter, session)
	if len(items) != 12 {
		t.Fatalf("rich items = %d, want 12", len(items))
	}

	assertFact(t, items[0], sessionio.FactKindLaunchDirectory, "/work/rich")
	assertFact(t, items[0], sessionio.FactKindGitRemote, "https://example.invalid/rich.git")
	assertFact(t, items[1], sessionio.FactKindWorkingDirectory, "/work/rich/turn")
	assertFact(t, items[1], sessionio.FactKindModel, "gpt-5.6-codex")
	assertFact(t, items[1], sessionio.FactKindSandboxPolicy, `{"type":"workspace-write","network_access":false}`)

	message := items[2].Events[0].Message
	if message == nil || message.Role != sessionio.MessageRoleUser || len(message.Content) != 4 {
		t.Fatalf("rich message = %#v", items[2].Events[0])
	}
	if message.Content[0].Text == nil || message.Content[0].Text.Text != "rich text" ||
		message.Content[1].Media == nil || message.Content[1].Availability != sessionio.ContentAvailabilityExternal ||
		message.Content[2].Media == nil || message.Content[2].Media.MediaType != "audio/wav" ||
		message.Content[2].Availability != sessionio.ContentAvailabilityAvailable ||
		string(message.Content[2].Media.Data) != "RIFF" || message.Content[2].Media.Reference != "" ||
		message.Content[3].Opaque == nil || message.Content[3].Opaque.NativeType != "future_content" {
		t.Fatalf("rich content = %#v", message.Content)
	}

	agentMessage := items[3].Events[0].Message
	if agentMessage == nil || agentMessage.Role != sessionio.MessageRoleAssistant ||
		len(agentMessage.Content) != 2 || agentMessage.Content[1].Opaque == nil ||
		agentMessage.Content[1].Availability != sessionio.ContentAvailabilityEncrypted {
		t.Fatalf("agent message = %#v", items[3].Events[0])
	}
	reasoning := items[4].Events[0].Reasoning
	if reasoning == nil || len(reasoning.Content) != 2 || len(reasoning.Summary) != 1 ||
		reasoning.Content[0].Text == nil || reasoning.Summary[0].Text == nil ||
		reasoning.Content[1].Availability != sessionio.ContentAvailabilityEncrypted {
		t.Fatalf("reasoning = %#v", items[4].Events[0])
	}
	if items[5].Events[0].Message == nil || len(items[5].Events[0].Message.Content) != 2 ||
		items[6].Events[0].Message == nil || items[6].Events[0].Message.Role != sessionio.MessageRoleAssistant ||
		items[7].Events[0].Reasoning == nil {
		t.Fatalf("event messages = %#v %#v %#v", items[5].Events, items[6].Events, items[7].Events)
	}

	localCall := items[8].Events[0].ToolCall
	if localCall == nil || localCall.Name != "local_shell" || localCall.CallID != "local-one" ||
		localCall.Input.MediaType != "application/json" || !strings.Contains(string(localCall.Input.Data), `"command":["pwd"]`) {
		t.Fatalf("local shell = %#v", items[8].Events[0])
	}
	customCall := items[9].Events[0].ToolCall
	if customCall == nil || customCall.Input.MediaType != "text/plain; charset=utf-8" ||
		!strings.Contains(string(customCall.Input.Data), "*** Begin Patch") {
		t.Fatalf("custom call = %#v", items[9].Events[0])
	}
	customResult := items[10].Events[0].ToolResult
	if customResult == nil || customResult.Output.MediaType != "application/json" ||
		len(items[10].Relations) != 1 || items[10].Relations[0].Kind != sessionio.RelationKindToolPair {
		t.Fatalf("custom result = %#v relations=%#v", items[10].Events[0], items[10].Relations)
	}
	operationalResult := items[11].Events[0].ToolResult
	if operationalResult == nil || operationalResult.Status != sessionio.ToolResultStatusSuccess ||
		operationalResult.Output.MediaType != "application/json" ||
		!strings.Contains(string(operationalResult.Output.Data), `"exit_code":0`) {
		t.Fatalf("operational result = %#v", items[11].Events[0])
	}
}

func TestLegacyDirectNormalization(t *testing.T) {
	adapter := newFixtureAdapter(t, fixtureHome(t))
	session := sessionByNativeID(t, collectSessions(t, adapter), "10000000-0000-4000-8000-000000000008")
	items := collectReadItems(t, adapter, session)
	if len(items) != 5 || items[0].Observation.NativeKind != "session_meta" {
		t.Fatalf("legacy items = %#v", items)
	}
	assertFact(t, items[0], sessionio.FactKindLaunchDirectory, "/work/legacy")
	assertFact(t, items[0], sessionio.FactKindGitBranch, "main")
	if items[1].Events[0].Message == nil || items[1].Events[0].Message.Content[0].Text.Text != "legacy hello" ||
		items[2].Events[0].Reasoning == nil || items[2].Events[0].Reasoning.Content[0].Text.Text != "legacy think" {
		t.Fatalf("legacy text events = %#v %#v", items[1].Events, items[2].Events)
	}
	call := items[3].Events[0].ToolCall
	result := items[4].Events[0].ToolResult
	if call == nil || call.Input.MediaType != "application/json" || string(call.Input.Data) != "{}" ||
		result == nil || result.Output.MediaType != "text/plain; charset=utf-8" || string(result.Output.Data) != "ok" ||
		len(items[4].Relations) != 1 {
		t.Fatalf("legacy tools = %#v %#v relations=%#v", items[3].Events, items[4].Events, items[4].Relations)
	}
}

func TestRichProjectionGolden(t *testing.T) {
	adapter := newFixtureAdapter(t, fixtureHome(t))
	session := sessionByIdentity(t, collectSessions(t, adapter), "session-rich")
	projections := make([]goldenProjection, 0, 12)
	for _, item := range collectReadItems(t, adapter, session) {
		projections = append(projections, projectGoldenItem(t, item))
	}
	actual, err := json.MarshalIndent(projections, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	expected, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex-rich.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("rich projection differs\nactual:\n%s\nexpected:\n%s", actual, expected)
	}
}

func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join("..", "..", "testdata", "codex")
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(home, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	return home
}

func newFixtureAdapter(t *testing.T, home string) *Adapter {
	t.Helper()
	adapter, err := New(Config{Home: home, MaxRecordBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}
func nextSource(t *testing.T, adapter *Adapter) sessionio.Source {
	t.Helper()
	stream, err := adapter.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	source, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return source
}
func collectSessions(t *testing.T, adapter *Adapter) []sessionio.SessionRef {
	t.Helper()
	stream, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var sessions []sessionio.SessionRef
	for {
		session, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return sessions
		}
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
}
func collectReadItems(t *testing.T, adapter *Adapter, session sessionio.SessionRef) []sessionio.ReadItem {
	t.Helper()
	stream, err := adapter.Read(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var items []sessionio.ReadItem
	for {
		item, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return items
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
}

func hasIdentity(session sessionio.SessionRef, value string) bool {
	for _, identity := range session.Native.Identities {
		if identity.Value == value {
			return true
		}
	}
	return false
}

func hasDiagnostic(diagnostics []sessionio.Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func sessionByIdentity(t *testing.T, sessions []sessionio.SessionRef, value string) sessionio.SessionRef {
	t.Helper()
	for _, session := range sessions {
		if hasIdentity(session, value) {
			return session
		}
	}
	t.Fatalf("session identity %q not found", value)
	return sessionio.SessionRef{}
}

func sessionByNativeID(t *testing.T, sessions []sessionio.SessionRef, value string) sessionio.SessionRef {
	t.Helper()
	for _, session := range sessions {
		if session.NativeID == value {
			return session
		}
	}
	t.Fatalf("native session %q not found", value)
	return sessionio.SessionRef{}
}

func assertFact(t *testing.T, item sessionio.ReadItem, kind sessionio.FactKind, value string) {
	t.Helper()
	if len(item.Events) != 1 || item.Events[0].Facts == nil {
		t.Fatalf("facts event missing: %#v", item.Events)
	}
	for _, fact := range item.Events[0].Facts.Facts {
		if fact.Kind == kind && fact.Value == value {
			return
		}
	}
	t.Fatalf("fact %s=%q missing from %#v", kind, value, item.Events[0].Facts.Facts)
}

type goldenProjection struct {
	NativeKind string                   `json:"native_kind"`
	Timestamp  string                   `json:"timestamp"`
	Kind       sessionio.EventKind      `json:"kind"`
	Role       sessionio.MessageRole    `json:"role,omitempty"`
	Content    []goldenContent          `json:"content,omitempty"`
	Summary    []goldenContent          `json:"summary,omitempty"`
	Facts      []sessionio.Fact         `json:"facts,omitempty"`
	ToolCall   *goldenToolCall          `json:"tool_call,omitempty"`
	ToolResult *goldenToolResult        `json:"tool_result,omitempty"`
	Relations  []sessionio.RelationKind `json:"relations,omitempty"`
}

type goldenContent struct {
	Kind         sessionio.ContentKind         `json:"kind"`
	Availability sessionio.ContentAvailability `json:"availability"`
	Text         string                        `json:"text,omitempty"`
	MediaType    string                        `json:"media_type,omitempty"`
	Reference    string                        `json:"reference,omitempty"`
	NativeType   string                        `json:"native_type,omitempty"`
	Data         string                        `json:"data,omitempty"`
}

type goldenToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type goldenToolResult struct {
	CallID    string                     `json:"call_id"`
	Status    sessionio.ToolResultStatus `json:"status"`
	MediaType string                     `json:"media_type"`
	Data      string                     `json:"data"`
}

func projectGoldenItem(t *testing.T, item sessionio.ReadItem) goldenProjection {
	t.Helper()
	if len(item.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(item.Events))
	}
	event := item.Events[0]
	projection := goldenProjection{NativeKind: item.Observation.NativeKind, Kind: event.Kind}
	if item.Observation.Timestamp != nil {
		projection.Timestamp = item.Observation.Timestamp.Format(time.RFC3339Nano)
	}
	if event.Message != nil {
		projection.Role = event.Message.Role
		projection.Content = projectGoldenContent(event.Message.Content)
	}
	if event.Reasoning != nil {
		projection.Content = projectGoldenContent(event.Reasoning.Content)
		projection.Summary = projectGoldenContent(event.Reasoning.Summary)
	}
	if event.Facts != nil {
		projection.Facts = append([]sessionio.Fact(nil), event.Facts.Facts...)
	}
	if event.ToolCall != nil {
		projection.ToolCall = &goldenToolCall{
			CallID:    event.ToolCall.CallID,
			Name:      event.ToolCall.Name,
			MediaType: event.ToolCall.Input.MediaType,
			Data:      string(event.ToolCall.Input.Data),
		}
	}
	if event.ToolResult != nil {
		projection.ToolResult = &goldenToolResult{
			CallID:    event.ToolResult.CallID,
			Status:    event.ToolResult.Status,
			MediaType: event.ToolResult.Output.MediaType,
			Data:      string(event.ToolResult.Output.Data),
		}
	}
	for _, relation := range item.Relations {
		projection.Relations = append(projection.Relations, relation.Kind)
	}
	return projection
}

func projectGoldenContent(blocks []sessionio.ContentBlock) []goldenContent {
	result := make([]goldenContent, 0, len(blocks))
	for _, block := range blocks {
		content := goldenContent{Kind: block.Kind, Availability: block.Availability}
		if block.Text != nil {
			content.Text = block.Text.Text
		}
		if block.Media != nil {
			content.MediaType = block.Media.MediaType
			content.Reference = block.Media.Reference
			content.Data = string(block.Media.Data)
		}
		if block.Opaque != nil {
			content.NativeType = block.Opaque.NativeType
			content.MediaType = block.Opaque.MediaType
			content.Data = string(block.Opaque.Data)
		}
		result = append(result, content)
	}
	return result
}

func paddedMetadataRecord(t *testing.T, size int, nativeID string) []byte {
	t.Helper()
	prefix := fmt.Sprintf(`{"id":%q,"type":"session_meta","padding":"`, nativeID)
	return paddedJSONRecord(t, size, prefix, `"}`)
}

func paddedUnknownRecord(t *testing.T, size int) []byte {
	t.Helper()
	return paddedJSONRecord(t, size, `{"type":"future_record","padding":"`, `"}`)
}

func paddedJSONRecord(t *testing.T, size int, prefix, suffix string) []byte {
	t.Helper()
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("record size %d is smaller than JSON framing %d", size, len(prefix)+len(suffix))
	}
	return []byte(prefix + strings.Repeat("x", padding) + suffix)
}

func writeRollout(t *testing.T, home string, active bool, name string, data []byte) string {
	t.Helper()
	directory := filepath.Join(home, "archived_sessions")
	if active {
		directory = filepath.Join(home, "sessions", "2026", "07", "24")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	fileName := filepath.Join(directory, name)
	if err := os.WriteFile(fileName, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return fileName
}

func assertReaderError(t *testing.T, err error, operation string) *sessionio.ReaderError {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil")
	}
	var readerError *sessionio.ReaderError
	if !errors.As(err, &readerError) {
		t.Fatalf("error type = %T, want *sessionio.ReaderError: %v", err, err)
	}
	if readerError.Operation != operation || readerError.Harness != sessionio.HarnessCodex ||
		readerError.AdapterVersion != adapterVersion {
		t.Fatalf("reader error context = %#v", readerError)
	}
	return readerError
}
