package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

func TestFixtureDiscoveryReadAndAuxiliaries(t *testing.T) {
	adapter, err := New(Config{ConfigDir: fixtureHome(t), MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	sources := collectSources(t, adapter)
	if len(sources) != 8 {
		t.Fatalf("sources = %d, want 8", len(sources))
	}
	if sources[0].Kind != sessionio.SourceKindCanonical || sources[0].Status != sessionio.SourceStatusAvailable {
		t.Fatalf("canonical source = %#v", sources[0])
	}
	if sources[0].Capabilities[3].Support != sessionio.SupportPartial || sources[0].Capabilities[8].Support != sessionio.SupportPartial {
		t.Fatalf("capabilities = %#v", sources[0].Capabilities)
	}
	for _, source := range sources[1:] {
		if source.Kind != sessionio.SourceKindAuxiliary || source.Status != sessionio.SourceStatusDisabled {
			t.Fatalf("auxiliary source = %#v", source)
		}
	}
	sessions := collectSessions(t, adapter)
	if len(sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(sessions))
	}
	primary := sessions[0]
	if primary.NativeID != "11111111-1111-4111-8111-111111111111" || primary.Title != "Fixture title" {
		t.Fatalf("primary = %#v", primary)
	}
	if sessions[1].NativeID != "worker" || sessions[1].Native.Relationships[0].TargetNativeID != "parent-agent" {
		t.Fatalf("direct subagent = %#v", sessions[1])
	}
	if sessions[2].NativeID != "flow" {
		t.Fatalf("workflow subagent = %#v", sessions[2])
	}
	directItems := collectItems(t, adapter, sessions[1])
	if len(directItems) != 2 || directItems[0].Observation.NativeKind != "agent_metadata" || directItems[1].Observation.NativeKind != "assistant" {
		t.Fatalf("sidecar ordering = %#v", directItems)
	}
	if len(directItems[1].Relations) != 1 || directItems[1].Relations[0].Kind != sessionio.RelationKindBranchParent {
		t.Fatalf("subagent fork relation = %#v", directItems[1].Relations)
	}
	items := collectItems(t, adapter, primary)
	if len(items) != 11 {
		t.Fatalf("items = %d, want 11", len(items))
	}
	if items[1].Events[0].Kind != sessionio.EventKindMessage || items[1].Events[1].Kind != sessionio.EventKindReasoning ||
		items[1].Events[2].Kind != sessionio.EventKindToolCall || items[1].Events[3].Kind != sessionio.EventKindToolCall ||
		items[1].Events[4].Kind != sessionio.EventKindUsage {
		t.Fatalf("assistant events = %#v", items[1].Events)
	}
	if len(items[2].Relations) != 3 || items[2].Relations[0].Kind != sessionio.RelationKindReplyTo ||
		items[2].Relations[1].Kind != sessionio.RelationKindToolPair || items[2].Relations[2].Kind != sessionio.RelationKindToolPair {
		t.Fatalf("relations = %#v", items[2].Relations)
	}
	if items[2].Relations[1].From.ID != string(items[1].Events[2].ID) ||
		items[2].Relations[1].To.ID != string(items[2].Events[1].ID) ||
		items[2].Relations[2].From.ID != string(items[1].Events[3].ID) ||
		items[2].Relations[2].To.ID != string(items[2].Events[2].ID) {
		t.Fatalf("tool pair order = %#v", items[2].Relations)
	}
	if len(items[2].Observation.Limitations) != 2 ||
		items[2].Observation.Limitations[0].Kind != sessionio.LimitationKindExternalPayload ||
		items[2].Observation.Limitations[1].Kind != sessionio.LimitationKindMissingExternalPayload ||
		items[2].Events[len(items[2].Events)-1].Marker.Name != "sidechain" {
		t.Fatalf("sidechain item = %#v", items[2])
	}
	if len(items[3].Events) != 2 || items[3].Events[0].Message == nil || items[3].Events[0].Message.Role != sessionio.MessageRoleSystem || items[3].Events[1].Marker == nil || items[3].Events[1].Marker.Name != "compaction" || items[5].Events[0].Message.Role != sessionio.MessageRoleSystem {
		t.Fatalf("operational projections = %#v %#v", items[3].Events, items[5].Events)
	}
	records := make([]sessionio.Record, 0, len(items))
	for _, item := range items {
		value := item
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindReadItem, ReadItem: &value})
	}
	if err := sessionio.WriteJSON(io.Discard, sessionio.Producer{Name: "claude-test", Version: "1"}, records); err != nil {
		t.Fatalf("normalized Claude records are invalid: %v", err)
	}
	var reconstructed []byte
	for index, item := range items {
		if item.Observation.Representation.Framing == nil && item.Observation.NativeKind == "agent_metadata" {
			continue
		}
		if item.Observation.Locator.File == nil || item.Observation.Locator.File.Record == nil || *item.Observation.Locator.File.Record != uint64(index+1) {
			t.Fatalf("locator %d = %#v", index, item.Observation.Locator)
		}
		reconstructed = append(reconstructed, item.Observation.Representation.Data...)
		reconstructed = append(reconstructed, item.Observation.Representation.Framing...)
	}
	want, err := os.ReadFile(filepath.Join(fixtureHome(t), "projects", "-workspace-demo", "11111111-1111-4111-8111-111111111111.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstructed, want) {
		t.Fatal("raw reconstruction differs")
	}
}

func TestRejectsIdentityMismatchAndMalformedContent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "projects", "-project")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"type":"user","sessionId":"other","message":{"content":"bad"}}` + "\n")
	if err := os.WriteFile(filepath.Join(path, "11111111-1111-4111-8111-111111111111.jsonl"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{}); err == nil {
		t.Fatal("Sessions() error = nil")
	}
	if err := os.WriteFile(filepath.Join(path, "11111111-1111-4111-8111-111111111111.jsonl"), []byte(`{"type":"assistant","sessionId":"11111111-1111-4111-8111-111111111111","message":{"role":"assistant","content":[{"type":"tool_use","id":"x"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := collectSessions(t, adapter)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	stream, err := adapter.Read(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var readerError *sessionio.ReaderError
	if err == nil {
		t.Fatal("Next() error = nil")
	}
	if !errors.As(err, &readerError) || readerError.Operation != "read" ||
		readerError.SessionID != sessions[0].ID || readerError.Locator == nil ||
		readerError.Locator.File == nil || readerError.Locator.File.Record == nil ||
		*readerError.Locator.File.Record != 1 ||
		!strings.Contains(err.Error(), "tool_use requires") {
		t.Fatalf("projection error = %#v", err)
	}
}

func TestKnownMalformedProjectionErrorsHaveLocators(t *testing.T) {
	for index, testCase := range []struct {
		name   string
		record string
		want   string
	}{
		{
			name:   "invalid base64 image",
			record: `{"type":"user","sessionId":"%s","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"***"}}]}}`,
			want:   "decode image base64",
		},
		{
			name:   "invalid usage counter type",
			record: `{"type":"assistant","sessionId":"%s","message":{"role":"assistant","usage":{"input_tokens":"bad"},"content":"answer"}}`,
			want:   "cannot unmarshal string",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			id := fmt.Sprintf("11111111-1111-4111-8111-%012d", 136+index)
			writeJSONL(t, filepath.Join(home, "projects", "-malformed", id+".jsonl"), fmt.Sprintf(testCase.record, id))
			adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
			if err != nil {
				t.Fatal(err)
			}
			session := collectSessions(t, adapter)[0]
			stream, err := adapter.Read(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			_, err = stream.Next(context.Background())
			var readerError *sessionio.ReaderError
			if !errors.As(err, &readerError) || readerError.Operation != "read" ||
				readerError.SessionID != session.ID || readerError.Locator == nil ||
				readerError.Locator.File == nil || readerError.Locator.File.Record == nil ||
				*readerError.Locator.File.Record != 1 || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("malformed projection error = %#v", err)
			}
		})
	}
}

func TestConfigLimits(t *testing.T) {
	if DefaultConfig().ConfigDir != "" || DefaultConfig().MaxRecordBytes != DefaultMaxRecordBytes {
		t.Fatalf("DefaultConfig() = %#v", DefaultConfig())
	}
	for _, limit := range []int64{0, -2} {
		if _, err := New(Config{ConfigDir: t.TempDir(), MaxRecordBytes: limit}); err == nil {
			t.Fatalf("New(%d) error = nil", limit)
		}
	}
	if _, err := New(Config{ConfigDir: t.TempDir(), MaxRecordBytes: sourceio.UnlimitedRecordBytes}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDirEnvironmentAndStreamLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	adapter, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if adapter.configDir != home {
		t.Fatalf("environment config dir = %q, want %q", adapter.configDir, home)
	}
	id := "11111111-1111-4111-8111-111111111123"
	writeJSONL(t, filepath.Join(home, "projects", "-lifecycle", id+".jsonl"), `{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"ok"}}`)
	session := collectSessions(t, adapter)[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Read(ctx, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Read = %v", err)
	}
	stream, err := adapter.Read(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	nextContext, cancelNext := context.WithCancel(context.Background())
	cancelNext()
	if _, err := stream.Next(nextContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next = %v", err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("Next after cancellation = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, sessionio.ErrStreamClosed) {
		t.Fatalf("closed Next = %v", err)
	}
}

func TestRecordLimitBoundaryAndUnlimited(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111124"
	record := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"boundary"}}`
	path := filepath.Join(home, "projects", "-boundary", id+".jsonl")
	writeJSONL(t, path, record)
	for _, test := range []struct {
		name  string
		limit int64
		ok    bool
	}{
		{"exact", int64(len(record)), true},
		{"over", int64(len(record) - 1), false},
		{"unlimited", sourceio.UnlimitedRecordBytes, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: test.limit})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Sessions(context.Background(), sessionio.SessionRequest{})
			if (err == nil) != test.ok {
				t.Fatalf("Sessions limit %d error = %v", test.limit, err)
			}
		})
	}
}

func TestValidationLimitsAndCancellation(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		adapter, err := New(Config{ConfigDir: t.TempDir(), MaxRecordBytes: DefaultMaxRecordBytes})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := adapter.Sources(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sources cancelled error = %v", err)
		}
	})
	t.Run("oversized transcript", func(t *testing.T) {
		home := t.TempDir()
		id := "11111111-1111-4111-8111-111111111115"
		writeJSONL(t, filepath.Join(home, "projects", "-limit", id+".jsonl"), `{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"this record is deliberately longer than the configured limit"}}`)
		adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: 32})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{}); err == nil {
			t.Fatal("Sessions() accepted an oversized record")
		}
	})
	t.Run("malformed sidecar", func(t *testing.T) {
		home := t.TempDir()
		primaryID := "11111111-1111-4111-8111-111111111116"
		path := filepath.Join(home, "projects", "-sidecar", primaryID, "subagents", "agent-worker.jsonl")
		writeJSONL(t, path, `{"type":"assistant","sessionId":"`+primaryID+`","agentId":"worker","message":{"role":"assistant","content":"work"}}`)
		writeFixtureFile(t, strings.TrimSuffix(path, ".jsonl")+".meta.json", []byte(`{`))
		adapter := newTestAdapter(t, home)
		_, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{})
		if err == nil {
			t.Fatal("Sessions() accepted a malformed agent sidecar")
		}
		var readerError *sessionio.ReaderError
		if !errors.As(err, &readerError) || readerError.Locator == nil || readerError.Locator.File == nil || !strings.HasSuffix(readerError.Locator.File.Path, ".meta.json") {
			t.Fatalf("sidecar error provenance = %#v", err)
		}
	})
}

func TestOperationalContentPreservesAllPersistedFields(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111117"
	writeJSONL(t, filepath.Join(home, "projects", "-operational", id+".jsonl"), `{"type":"system","sessionId":"`+id+`","subtype":"compact_boundary","content":"content","attachment":"attachment","hookAdditionalContext":"hook"}`)
	adapter := newTestAdapter(t, home)
	items := collectItems(t, adapter, collectSessions(t, adapter)[0])
	events := items[0].Events
	if len(events) != 4 || events[0].Message.Content[0].Text.Text != "content" || events[1].Message.Content[0].Text.Text != "attachment" || events[2].Message.Content[0].Text.Text != "hook" || events[3].Marker == nil || events[3].Marker.Name != "compaction" {
		t.Fatalf("operational events = %#v", events)
	}
}

func TestDiscoveryFilteringStableIDsAndSourceStatuses(t *testing.T) {
	home := t.TempDir()
	missing, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	sources := collectSources(t, missing)
	if sources[0].Status != sessionio.SourceStatusMissing || sources[1].Status != sessionio.SourceStatusMissing {
		t.Fatalf("missing source statuses = %#v", sources[:2])
	}
	project := filepath.Join(home, "projects", "-copies")
	firstID := "11111111-1111-4111-8111-111111111118"
	secondID := "11111111-1111-4111-8111-111111111119"
	writeJSONL(t, filepath.Join(project, secondID+".jsonl"), `{"type":"user","sessionId":"`+secondID+`","message":{"role":"user","content":"second"}}`)
	writeJSONL(t, filepath.Join(project, firstID+".jsonl"), `{"type":"user","sessionId":"`+firstID+`","message":{"role":"user","content":"first"}}`)
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	refs := collectSessions(t, adapter)
	if len(refs) != 2 || refs[0].NativeID != firstID || refs[1].NativeID != secondID {
		t.Fatalf("lexical sessions = %#v", refs)
	}
	again := collectSessions(t, adapter)
	if refs[0].ID != again[0].ID || refs[0].DiscoveryRevision != again[0].DiscoveryRevision {
		t.Fatal("session IDs or discovery revision are unstable")
	}
	filtered, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{Sources: []sessionio.SourceID{"other"}})
	if err != nil {
		t.Fatal(err)
	}
	if values := collectStream(t, filtered); len(values) != 0 {
		t.Fatalf("foreign source filter = %#v", values)
	}
	writeJSONL(t, filepath.Join(project, firstID+".jsonl"), `{"type":"user","sessionId":"`+firstID+`","message":{"role":"user","content":"first"}}`, `{"type":"ai-title","aiTitle":"refresh"}`)
	if refreshed := collectSessions(t, adapter)[0]; refreshed.ID != refs[0].ID || refreshed.DiscoveryRevision == refs[0].DiscoveryRevision {
		t.Fatalf("refresh = %#v, old = %#v", refreshed, refs[0])
	}
}

func TestExactIDFramingInputs(t *testing.T) {
	const (
		root       = "/synthetic/claude"
		relative   = "projects/-demo/11111111-1111-4111-8111-111111111111.jsonl"
		nativeID   = "11111111-1111-4111-8111-111111111111"
		sidecar    = "projects/-demo/x/subagents/agent-a.meta.json"
		wantSource = "source:sha256:720f9696f877bd39983f6b04604323b4b3232b44235c72682d3a96aa14547638"
		wantAux    = "source:sha256:b76f18212122b881aeb33686c21fe9c088fc502eb8d1e3ce45184030c542f007"
		wantOcc    = "occurrence:sha256:e670da198114a8b8fdc6c2b50e3eb9497dd61628949421fdc53918729c3cbd47"
		wantSess   = "session:sha256:265dd89ab286042e798a0398c37f265681521206ad6b9f3ee122e5b6cd947534"
		wantJSONL  = "observation:sha256:4d657ace3dc1bbd6fcf27eb713dc7b8d5880b90f959a1f8734a91b90ab2267ff"
		wantMeta   = "observation:sha256:38a5651cf4c5369ed159d499eff1879ec8999f136d37822a2f3e8008406e2002"
	)
	adapter := newTestAdapter(t, root)
	if string(adapter.sourceID) != wantSource {
		t.Fatalf("source ID = %q", adapter.sourceID)
	}
	if got := derivedID("source", string(sessionio.HarnessClaude), root, "history.jsonl"); got != wantAux {
		t.Fatalf("auxiliary source ID = %q", got)
	}
	if got := string(adapter.occurrenceID(occurrence{relative: relative})); got != wantOcc {
		t.Fatalf("occurrence ID = %q", got)
	}
	if got := derivedID("session", wantOcc, nativeID); got != wantSess {
		t.Fatalf("session ID = %q", got)
	}
	if got := derivedID("observation", wantSess, "jsonl", "1", digest([]byte(`{"type":"user"}`), []byte("\n"))); got != wantJSONL {
		t.Fatalf("JSONL observation ID = %q", got)
	}
	if got := derivedID("observation", wantSess, "meta", sidecar, digest([]byte("{\"model\":\"m\"}\n"))); got != wantMeta {
		t.Fatalf("sidecar observation ID = %q", got)
	}
}

func TestSymlinkAndWorkflowJournalAreNotCanonical(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "projects", "-links")
	id := "11111111-1111-4111-8111-111111111120"
	writeJSONL(t, filepath.Join(project, id+".jsonl"), `{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"ok"}}`)
	if err := os.Symlink(filepath.Join(project, id+".jsonl"), filepath.Join(project, "22222222-2222-4222-8222-222222222222.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	journal := filepath.Join(project, id, "subagents", "workflows", "flow", "journal.jsonl")
	writeJSONL(t, journal, `{"not":"canonical"}`)
	symlinkedSession := "33333333-3333-4333-8333-333333333333"
	if err := os.Symlink(filepath.Join(project, id), filepath.Join(project, symlinkedSession)); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	if refs := collectSessions(t, adapter); len(refs) != 1 {
		t.Fatalf("symlink/journal became sessions: %#v", refs)
	}
	sources := collectSources(t, adapter)
	if len(sources[0].Diagnostics) == 0 || len(sources) != 8 || sources[len(sources)-1].Status != sessionio.SourceStatusDisabled {
		t.Fatalf("sources = %#v", sources)
	}
	wantSymlinkPath := filepath.ToSlash(filepath.Join("projects", "-links", symlinkedSession))
	foundSymlinkDiagnostic := false
	for _, diagnostic := range sources[0].Diagnostics {
		if diagnostic.Locator != nil && diagnostic.Locator.File != nil && diagnostic.Locator.File.Path == wantSymlinkPath {
			foundSymlinkDiagnostic = true
		}
	}
	if !foundSymlinkDiagnostic {
		t.Fatalf("missing symlinked session-directory diagnostic in %#v", sources[0].Diagnostics)
	}
}

func TestRelationDiagnosticsAndReverseToolPair(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111121"
	path := filepath.Join(home, "projects", "-relations", id+".jsonl")
	writeJSONL(t, path,
		`{"type":"user","uuid":"duplicate","sessionId":"`+id+`","message":{"role":"user","content":"one"}}`,
		`{"type":"user","uuid":"duplicate","sessionId":"`+id+`","message":{"role":"user","content":"two"}}`,
		`{"type":"user","parentUuid":"duplicate","sessionId":"`+id+`","message":{"role":"user","content":"ambiguous parent"}}`,
		`{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"reverse","content":"done"}]}}`,
		`{"type":"assistant","sessionId":"`+id+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"reverse","name":"run","input":{}}]}}`,
		`{"type":"assistant","sessionId":"`+id+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"duplicate-tool","name":"run","input":{}}]}}`,
		`{"type":"assistant","sessionId":"`+id+`","message":{"role":"assistant","content":[{"type":"tool_use","id":"duplicate-tool","name":"run","input":{}}]}}`,
	)
	adapter := newTestAdapter(t, home)
	items := collectItems(t, adapter, collectSessions(t, adapter)[0])
	if !hasDiagnostic(items[2].Diagnostics, "claude_parent_unresolved") {
		t.Fatalf("parent diagnostics = %#v", items[2].Diagnostics)
	}
	if len(items[4].Relations) != 1 || items[4].Relations[0].Kind != sessionio.RelationKindToolPair {
		t.Fatalf("reverse pair = %#v", items[4].Relations)
	}
	if !hasDiagnostic(items[5].Diagnostics, "claude_tool_pair_unresolved") || !hasDiagnostic(items[6].Diagnostics, "claude_tool_pair_unresolved") {
		t.Fatalf("duplicate tool diagnostics = %#v %#v", items[5].Diagnostics, items[6].Diagnostics)
	}
}

func TestProjectionMarkersAndUnknownRecords(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111122"
	writeJSONL(t, filepath.Join(home, "projects", "-projection", id+".jsonl"),
		`{"type":"assistant","sessionId":"`+id+`","isCompactSummary":true,"message":{"role":"user","content":"summary"}}`,
		`{"type":"user","sessionId":"`+id+`","isSidechain":false,"message":{"role":"user","content":"branch"}}`,
		`{"type":"system","sessionId":"`+id+`","subtype":"future","content":"preserved"}`,
		`{"type":"future-root","sessionId":"`+id+`"}`,
		`{"type":"attachment","sessionId":"`+id+`","attachment":"attached"}`,
	)
	adapter := newTestAdapter(t, home)
	items := collectItems(t, adapter, collectSessions(t, adapter)[0])
	if items[0].Events[0].Message.Role != sessionio.MessageRoleSystem || items[0].Events[1].Marker.Name != "compaction" || items[1].Events[1].Marker.State != "false" {
		t.Fatalf("compact/sidechain = %#v %#v", items[0].Events, items[1].Events)
	}
	if items[2].Events[0].Message.Content[0].Text.Text != "preserved" || items[2].Events[1].Unknown == nil || !hasDiagnostic(items[2].Diagnostics, "claude_unknown_system_subtype") {
		t.Fatalf("unknown system = %#v", items[2])
	}
	if items[3].Events[0].Unknown == nil || !hasDiagnostic(items[3].Diagnostics, "claude_unknown_record_type") || items[4].Events[1].Marker.Name != "attachment" {
		t.Fatalf("unknown/attachment = %#v %#v", items[3], items[4])
	}
}

func TestSidecarAndExternalPayloadBoundaries(t *testing.T) {
	home := t.TempDir()
	primary := "11111111-1111-4111-8111-111111111125"
	path := filepath.Join(home, "projects", "-sidecar-boundary", primary, "subagents", "agent-x.jsonl")
	writeJSONL(t, path, `{"type":"assistant","sessionId":"`+primary+`","agentId":"x","message":{"role":"assistant","content":"ok"}}`)
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	meta := []byte("{\"model\":\"m\"}\n")
	if err := os.WriteFile(metaPath, meta, 0o644); err != nil {
		t.Fatal(err)
	}
	adapter := newTestAdapter(t, home)
	items := collectItems(t, adapter, collectSessions(t, adapter)[0])
	metaDigest := sha256.Sum256(meta)
	if !bytes.Equal(items[0].Observation.Representation.Data, meta) ||
		items[0].Observation.Representation.Framing != nil ||
		items[0].Observation.Locator.File.Path != strings.TrimPrefix(metaPath, home+string(filepath.Separator)) ||
		items[0].Observation.Revision.Value != fmt.Sprintf("sha256:%x", metaDigest) {
		t.Fatalf("sidecar evidence = %#v", items[0].Observation)
	}
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if got := collectItems(t, adapter, collectSessions(t, adapter)[0]); len(got) != 1 {
		t.Fatalf("absent sidecar items = %d", len(got))
	}
	oversized := []byte(`{"padding":"` + strings.Repeat("x", 300) + `"}`)
	if err := os.WriteFile(metaPath, oversized, 0o644); err != nil {
		t.Fatal(err)
	}
	limited, err := New(Config{ConfigDir: home, MaxRecordBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Sessions(context.Background(), sessionio.SessionRequest{}); err == nil {
		t.Fatal("oversized sidecar accepted")
	}
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, metaPath); err == nil {
		_, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{})
		var readerError *sessionio.ReaderError
		if err == nil {
			t.Fatal("symlink sidecar accepted")
		}
		if !errors.As(err, &readerError) || readerError.Locator == nil ||
			readerError.Locator.File == nil || readerError.Locator.File.Path !=
			strings.TrimPrefix(metaPath, home+string(filepath.Separator)) {
			t.Fatalf("symlink sidecar error = %#v", err)
		}
	}
}

func TestSidecarMutationAfterReadFailsBeforeEmission(t *testing.T) {
	home := t.TempDir()
	primary := "11111111-1111-4111-8111-111111111126"
	path := filepath.Join(home, "projects", "-sidecar-mutation", primary, "subagents", "agent-x.jsonl")
	writeJSONL(t, path, `{"type":"assistant","sessionId":"`+primary+`","agentId":"x","message":{"role":"assistant","content":"ok"}}`)
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	writeFixtureFile(t, metaPath, []byte(`{"model":"one"}`))
	adapter := newTestAdapter(t, home)
	stream := openItemStream(t, adapter, collectSessions(t, adapter)[0])
	defer stream.Close()
	if err := os.WriteFile(metaPath, []byte(`{"model":"changed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("mutated sidecar emitted")
	} else {
		var reader *sessionio.ReaderError
		if !errors.As(err, &reader) || reader.Locator == nil || reader.Locator.File == nil || !strings.HasSuffix(reader.Locator.File.Path, ".meta.json") {
			t.Fatalf("sidecar mutation error = %#v", err)
		}
	}
}

func TestRichContentUsageAndExternalPayloads(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111126"
	if err := os.MkdirAll(filepath.Join(home, "tool-results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "tool-results", "present.txt"), []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(home, "projects", "-rich", id+".jsonl"),
		`{"type":"assistant","sessionId":"`+id+`","message":{"role":"assistant","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"total_tokens":18,"cache_creation":{"ephemeral_5m_input_tokens":99},"server_tool_use":{"web_search_requests":4}},"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"UklGRg=="}},{"type":"image","source":{"type":"url","media_type":"image/png","url":"https://example.invalid/image.png"}},{"type":"future","value":1}]}}`,
		`{"type":"assistant","sessionId":"`+id+`","message":{"role":"assistant","usage":{"cache_creation":{"ephemeral_5m_input_tokens":99},"server_tool_use":{"web_search_requests":4}},"content":"nested only"}}`,
		`{"type":"user","sessionId":"`+id+`","toolUseResult":{"persistedOutputPath":"tool-results/present.txt"},"message":{"role":"user","content":"present"}}`,
		`{"type":"user","sessionId":"`+id+`","toolUseResult":{"persistedOutputPath":"tool-results/missing.txt"},"message":{"role":"user","content":"missing"}}`,
	)
	adapter := newTestAdapter(t, home)
	items := collectItems(t, adapter, collectSessions(t, adapter)[0])
	if len(items) != 4 {
		t.Fatalf("rich items = %d, want 4", len(items))
	}
	message := items[0].Events[0].Message
	if message == nil || len(message.Content) != 3 ||
		message.Content[0].Media == nil ||
		message.Content[0].Availability != sessionio.ContentAvailabilityAvailable ||
		string(message.Content[0].Media.Data) != "RIFF" ||
		message.Content[1].Media == nil ||
		message.Content[1].Availability != sessionio.ContentAvailabilityExternal ||
		message.Content[1].Media.Reference != "https://example.invalid/image.png" ||
		message.Content[2].Opaque == nil || message.Content[2].Opaque.NativeType != "future" {
		t.Fatalf("rich content = %#v", message)
	}
	var usage *sessionio.UsageEvent
	for _, event := range items[0].Events {
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 11 ||
		usage.OutputTokens == nil || *usage.OutputTokens != 7 ||
		usage.CacheReadTokens == nil || *usage.CacheReadTokens != 3 ||
		usage.CacheWriteTokens == nil || *usage.CacheWriteTokens != 2 ||
		usage.TotalTokens == nil || *usage.TotalTokens != 18 {
		t.Fatalf("usage = %#v", usage)
	}
	for _, event := range items[1].Events {
		if event.Usage != nil {
			t.Fatalf("nested-only usage normalized = %#v", event.Usage)
		}
	}
	if len(items[2].Observation.Limitations) != 1 ||
		items[2].Observation.Limitations[0].Kind != sessionio.LimitationKindExternalPayload ||
		len(items[3].Observation.Limitations) != 1 ||
		items[3].Observation.Limitations[0].Kind != sessionio.LimitationKindMissingExternalPayload {
		t.Fatalf("external limitations = %#v %#v", items[2].Observation.Limitations, items[3].Observation.Limitations)
	}

	negativeHome := t.TempDir()
	negativeID := "11111111-1111-4111-8111-111111111127"
	writeJSONL(t, filepath.Join(negativeHome, "projects", "-usage", negativeID+".jsonl"),
		`{"type":"assistant","sessionId":"`+negativeID+`","message":{"role":"assistant","usage":{"input_tokens":-1},"content":"bad"}}`,
	)
	negative, err := New(Config{ConfigDir: negativeHome, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := negative.Read(context.Background(), collectSessions(t, negative)[0])
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("negative usage accepted")
	}
}

func TestExternalPayloadUnexpectedFileTypeFails(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111144"
	if err := os.MkdirAll(filepath.Join(home, "tool-results", "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(home, "projects", "-external", id+".jsonl"),
		`{"type":"user","sessionId":"`+id+`","toolUseResult":{"persistedOutputPath":"tool-results/directory"},"message":{"role":"user","content":"payload"}}`,
	)
	adapter := newTestAdapter(t, home)
	session := collectSessions(t, adapter)[0]
	stream := openItemStream(t, adapter, session)
	defer stream.Close()
	_, err := stream.Next(context.Background())
	var readerError *sessionio.ReaderError
	if !errors.As(err, &readerError) || readerError.Locator == nil ||
		readerError.Locator.File == nil || readerError.Locator.File.Record == nil ||
		*readerError.Locator.File.Record != 1 ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("external payload error = %#v", err)
	}
}

func TestGrowingTailMutationAndTimestampDiagnostics(t *testing.T) {
	t.Run("pending tail", func(t *testing.T) {
		home := t.TempDir()
		id := "11111111-1111-4111-8111-111111111128"
		first := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"complete"}}`
		pending := `{"type":"assistant","sessionId":"` + id + `","message":{"role":"assistant","content":"pending"}}`
		path := filepath.Join(home, "projects", "-tail", id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(first+"\n"+pending), 0o644); err != nil {
			t.Fatal(err)
		}
		adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
		if err != nil {
			t.Fatal(err)
		}
		session := collectSessions(t, adapter)[0]
		if items := collectItems(t, adapter, session); len(items) != 1 {
			t.Fatalf("pending generation items = %d, want 1", len(items))
		}
		if err := os.WriteFile(path, []byte(first+"\n"+pending+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if items := collectItems(t, adapter, session); len(items) != 2 {
			t.Fatalf("completed generation items = %d, want 2", len(items))
		}
	})

	t.Run("mutation", func(t *testing.T) {
		home := t.TempDir()
		id := "11111111-1111-4111-8111-111111111129"
		path := filepath.Join(home, "projects", "-mutation", id+".jsonl")
		writeJSONL(t, path, `{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"before"}}`)
		adapter := newTestAdapter(t, home)
		stream := openItemStream(t, adapter, collectSessions(t, adapter)[0])
		defer stream.Close()
		writeJSONL(t, path, `{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"after!"}}`)
		_, err := stream.Next(context.Background())
		var changed *sourceio.ChangedSourceError
		var readerError *sessionio.ReaderError
		if !errors.As(err, &changed) || !errors.As(err, &readerError) ||
			readerError.Locator == nil || readerError.Locator.File == nil ||
			readerError.Locator.File.Record == nil || *readerError.Locator.File.Record != 1 {
			t.Fatalf("mutation error = %#v", err)
		}
	})

	t.Run("invalid timestamp", func(t *testing.T) {
		home := t.TempDir()
		id := "11111111-1111-4111-8111-111111111130"
		writeJSONL(t, filepath.Join(home, "projects", "-timestamp", id+".jsonl"),
			`{"type":"user","sessionId":"`+id+`","timestamp":"not-a-time","message":{"role":"user","content":"kept"}}`,
		)
		adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
		if err != nil {
			t.Fatal(err)
		}
		session := collectSessions(t, adapter)[0]
		if session.StartedAt != nil || !hasDiagnostic(session.Diagnostics, "claude_invalid_timestamp") {
			t.Fatalf("session timestamp = %#v diagnostics=%#v", session.StartedAt, session.Diagnostics)
		}
		item := collectItems(t, adapter, session)[0]
		if item.Observation.Timestamp != nil ||
			!hasDiagnostic(item.Diagnostics, "claude_invalid_timestamp") ||
			item.Observation.Locator.File == nil || item.Observation.Locator.File.Record == nil ||
			*item.Observation.Locator.File.Record != 1 {
			t.Fatalf("timestamp item = %#v", item)
		}
	})
}

func TestReadMissingTranscriptHasLocator(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111148"
	path := filepath.Join(home, "projects", "-missing-read", id+".jsonl")
	writeJSONL(t, path, `{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"present"}}`)
	adapter := newTestAdapter(t, home)
	session := collectSessions(t, adapter)[0]
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := adapter.Read(context.Background(), session)
	var readerError *sessionio.ReaderError
	if !errors.As(err, &readerError) || readerError.Locator == nil ||
		readerError.Locator.File == nil ||
		readerError.Locator.File.Path != session.Occurrence.Locator.File.Path {
		t.Fatalf("missing transcript error = %#v", err)
	}
}

func TestMissingAndAmbiguousTopologyDiagnostics(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "projects", "-topology")
	firstParent := "11111111-1111-4111-8111-111111111131"
	secondParent := "11111111-1111-4111-8111-111111111132"
	missingChild := "11111111-1111-4111-8111-111111111133"
	ambiguousChild := "11111111-1111-4111-8111-111111111134"
	writeJSONL(t, filepath.Join(project, firstParent+".jsonl"),
		`{"type":"user","uuid":"ambiguous-target","sessionId":"`+firstParent+`","message":{"role":"user","content":"one"}}`,
	)
	writeJSONL(t, filepath.Join(project, secondParent+".jsonl"),
		`{"type":"user","uuid":"ambiguous-target","sessionId":"`+secondParent+`","message":{"role":"user","content":"two"}}`,
	)
	writeJSONL(t, filepath.Join(project, missingChild+".jsonl"),
		`{"type":"user","uuid":"missing-child","parentUuid":"missing-parent","sessionId":"`+missingChild+`","forkedFrom":{"sessionId":"`+firstParent+`","messageUuid":"missing-target"},"message":{"role":"user","content":"missing"}}`,
	)
	writeJSONL(t, filepath.Join(project, ambiguousChild+".jsonl"),
		`{"type":"user","uuid":"ambiguous-child","sessionId":"`+ambiguousChild+`","forkedFrom":{"sessionId":"`+firstParent+`","messageUuid":"ambiguous-target"},"message":{"role":"user","content":"ambiguous"}}`,
	)
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	sessions := collectSessions(t, adapter)
	missingItems := collectItems(t, adapter, sessionByNativeID(t, sessions, missingChild))
	if len(missingItems[0].Relations) != 0 ||
		!hasDiagnostic(missingItems[0].Diagnostics, "claude_parent_unresolved") ||
		!hasDiagnostic(missingItems[0].Diagnostics, "claude_fork_target_unresolved") {
		t.Fatalf("missing topology = %#v", missingItems[0])
	}
	ambiguousItems := collectItems(t, adapter, sessionByNativeID(t, sessions, ambiguousChild))
	if len(ambiguousItems[0].Relations) != 0 ||
		!hasDiagnostic(ambiguousItems[0].Diagnostics, "claude_fork_target_unresolved") {
		t.Fatalf("ambiguous topology = %#v", ambiguousItems[0])
	}
}

func TestCopiedOccurrencesAndFreshReadRevision(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111135"
	record := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"copy"}}`
	firstPath := filepath.Join(home, "projects", "-copy-a", id+".jsonl")
	writeJSONL(t, firstPath, record)
	writeJSONL(t, filepath.Join(home, "projects", "-copy-b", id+".jsonl"), record)
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	sessions := collectSessions(t, adapter)
	if len(sessions) != 2 || sessions[0].NativeID != id || sessions[1].NativeID != id ||
		sessions[0].ID == sessions[1].ID || sessions[0].Occurrence.ID == sessions[1].Occurrence.ID {
		t.Fatalf("copied occurrences = %#v", sessions)
	}
	firstItems := collectItems(t, adapter, sessions[0])
	secondItems := collectItems(t, adapter, sessions[1])
	if firstItems[0].Observation.ID == secondItems[0].Observation.ID ||
		!bytes.Equal(firstItems[0].Observation.Representation.Data, secondItems[0].Observation.Representation.Data) {
		t.Fatalf("copied observations = %#v %#v", firstItems[0], secondItems[0])
	}
	stale := sessions[0]
	writeJSONL(t, firstPath, record, `{"type":"ai-title","aiTitle":"fresh"}`)
	freshItems := collectItems(t, adapter, stale)
	if freshItems[0].Session.ID != stale.ID ||
		freshItems[0].Session.DiscoveryRevision == stale.DiscoveryRevision ||
		freshItems[0].Session.Title != "fresh" {
		t.Fatalf("fresh read session = %#v, stale = %#v", freshItems[0].Session, stale)
	}
}

func TestTitlePrecedenceAndIdentityOptionalAfterHeader(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111112"
	path := filepath.Join(home, "projects", "-title", id+".jsonl")
	writeJSONL(t, path,
		`{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"start"}}`,
		`{"type":"ai-title","aiTitle":"AI first"}`,
		`{"type":"last-prompt","content":"not a title"}`,
		`{"type":"ai-title","aiTitle":"AI last"}`,
		`{"type":"custom-title","customTitle":"Custom first"}`,
		`{"type":"custom-title","customTitle":"Custom last"}`,
		`{"type":"file-history-delta","content":{"path":"without IDs"}}`,
	)
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	sessions := collectSessions(t, adapter)
	if len(sessions) != 1 || sessions[0].Title != "Custom last" {
		t.Fatalf("title sessions = %#v", sessions)
	}
	if items := collectItems(t, adapter, sessions[0]); len(items) != 7 {
		t.Fatalf("items = %d, want 7", len(items))
	}
}

func TestIDLessOperationalFirstRecordUsesFilenameIdentity(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111145"
	writeJSONL(t, filepath.Join(home, "projects", "-idless-header", id+".jsonl"),
		`{"type":"local_command","content":"setup"}`,
		`{"type":"user","sessionId":"`+id+`","message":{"role":"user","content":"prompt"}}`,
	)
	adapter := newTestAdapter(t, home)
	sessions := collectSessions(t, adapter)
	if len(sessions) != 1 || sessions[0].NativeID != id {
		t.Fatalf("ID-less header session = %#v", sessions)
	}
	if items := collectItems(t, adapter, sessions[0]); len(items) != 2 {
		t.Fatalf("ID-less header items = %#v", items)
	}
}

func TestDiscoveryRevisionIncludesSelectedTitleEvidence(t *testing.T) {
	home := t.TempDir()
	id := "11111111-1111-4111-8111-111111111143"
	path := filepath.Join(home, "projects", "-title-evidence", id+".jsonl")
	header := `{"type":"user","sessionId":"` + id + `","message":{"role":"user","content":"start"}}`
	writeJSONL(t, path, header, `{"type":"ai-title","aiTitle":"same","padding":"a"}`)
	adapter := newTestAdapter(t, home)
	before := collectSessions(t, adapter)[0]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, path, header, `{"type":"ai-title","aiTitle":"same","padding":"b"}`)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after := collectSessions(t, adapter)[0]
	if after.Title != before.Title || after.DiscoveryRevision == before.DiscoveryRevision {
		t.Fatalf("title evidence revision before=%#v after=%#v", before, after)
	}
}

func TestForkBranchParentAcrossProject(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "projects", "-fork")
	parentID := "11111111-1111-4111-8111-111111111113"
	childID := "11111111-1111-4111-8111-111111111114"
	writeJSONL(t, filepath.Join(project, parentID+".jsonl"), `{"type":"user","uuid":"parent-message","sessionId":"`+parentID+`","message":{"role":"user","content":"parent"}}`)
	writeJSONL(t, filepath.Join(project, childID+".jsonl"),
		`{"type":"user","uuid":"child-start","sessionId":"`+childID+`","message":{"role":"user","content":"child"}}`,
		`{"type":"assistant","uuid":"child-message","sessionId":"`+childID+`","forkedFrom":{"sessionId":"`+parentID+`","messageUuid":"parent-message"},"message":{"role":"assistant","content":"forked"}}`,
	)
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range collectSessions(t, adapter) {
		if session.NativeID != childID {
			continue
		}
		if len(session.Native.Relationships) != 1 || session.Native.Relationships[0].Kind != sessionio.NativeRelationshipKindForkParent {
			t.Fatalf("fork hint = %#v", session.Native)
		}
		items := collectItems(t, adapter, session)
		if len(items[1].Relations) != 1 || items[1].Relations[0].Kind != sessionio.RelationKindBranchParent {
			t.Fatalf("fork relation = %#v", items[1].Relations)
		}
	}
}

func TestForkTargetIdentityFailureUsesSiblingProvenance(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "projects", "-fork-provenance")
	parentID := "11111111-1111-4111-8111-111111111146"
	childID := "11111111-1111-4111-8111-111111111147"
	parentPath := filepath.Join(project, parentID+".jsonl")
	writeJSONL(t, parentPath, `{"type":"user","uuid":"target","sessionId":"`+parentID+`","message":{"role":"user","content":"parent"}}`)
	writeJSONL(t, filepath.Join(project, childID+".jsonl"),
		`{"type":"user","uuid":"child","sessionId":"`+childID+`","forkedFrom":{"sessionId":"`+parentID+`","messageUuid":"target"},"message":{"role":"user","content":"child"}}`,
	)
	adapter := newTestAdapter(t, home)
	child := sessionByNativeID(t, collectSessions(t, adapter), childID)
	stream := openItemStream(t, adapter, child)
	defer stream.Close()
	writeJSONL(t, parentPath, `{"type":"user","uuid":"target","sessionId":"wrong","message":{"role":"user","content":"parent"}}`)
	_, err := stream.Next(context.Background())
	var readerError *sessionio.ReaderError
	wantPath := filepath.ToSlash(filepath.Join("projects", "-fork-provenance", parentID+".jsonl"))
	if !errors.As(err, &readerError) || readerError.Locator == nil ||
		readerError.Locator.File == nil || readerError.Locator.File.Path != wantPath ||
		readerError.Locator.File.Record == nil || *readerError.Locator.File.Record != 1 ||
		!strings.Contains(err.Error(), "fork target transcript identity") {
		t.Fatalf("fork target provenance error = %#v", err)
	}
}

func TestRichProjectionGolden(t *testing.T) {
	adapter, err := New(Config{ConfigDir: fixtureHome(t), MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	sessions := collectSessions(t, adapter)
	records := make([]sessionio.Record, 0)
	for _, source := range collectSources(t, adapter) {
		value := source
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindSource, Source: &value})
	}
	for _, session := range sessions {
		value := session
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindSession, Session: &value})
		for _, item := range collectItems(t, adapter, session) {
			value := item
			records = append(records, sessionio.Record{Kind: sessionio.RecordKindReadItem, ReadItem: &value})
		}
	}
	var document bytes.Buffer
	if err := sessionio.WriteJSON(&document, sessionio.Producer{Name: "claude-golden", Version: "1"}, records); err != nil {
		t.Fatal(err)
	}
	sanitized, err := sanitizedSnapshot(document.Bytes(), adapter.configDir)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	goldenPath := filepath.Join("..", "..", "testdata", "claude-rich.golden.json")
	if os.Getenv("UPDATE_CLAUDE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, actual, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Claude full snapshot differs\nactual:\n%s\nexpected:\n%s", actual, expected)
	}
}

func sanitizedSnapshot(encoded []byte, root string) (any, error) {
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	tokens := map[string]string{}
	next := 0
	var sanitize func(any) any
	sanitize = func(item any) any {
		switch current := item.(type) {
		case string:
			current = strings.ReplaceAll(current, root, "$CONFIG_ROOT")
			if strings.Contains(current, ":sha256:") || strings.HasPrefix(current, "sha256:") {
				if token, found := tokens[current]; found {
					return token
				}
				next++
				token := fmt.Sprintf("$OPAQUE_%03d", next)
				tokens[current] = token
				return token
			}
			return current
		case []any:
			for index := range current {
				current[index] = sanitize(current[index])
			}
			return current
		case map[string]any:
			keys := make([]string, 0, len(current))
			for key := range current {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				nested := current[key]
				current[key] = sanitize(nested)
			}
			return current
		default:
			return item
		}
	}
	return sanitize(value), nil
}

func fixtureHome(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "claude")
}

func writeJSONL(t *testing.T, path string, records ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, []byte(strings.Join(records, "\n")+"\n"))
}

func writeFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestAdapter(t *testing.T, home string) *Adapter {
	t.Helper()
	adapter, err := New(Config{ConfigDir: home, MaxRecordBytes: DefaultMaxRecordBytes})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func collectSources(t *testing.T, adapter *Adapter) []sessionio.Source {
	t.Helper()
	return collectOpened(t, func() (sessionio.Stream[sessionio.Source], error) {
		return adapter.Sources(context.Background())
	})
}

func collectSessions(t *testing.T, adapter *Adapter) []sessionio.SessionRef {
	t.Helper()
	return collectOpened(t, func() (sessionio.Stream[sessionio.SessionRef], error) {
		return adapter.Sessions(context.Background(), sessionio.SessionRequest{})
	})
}

func collectItems(t *testing.T, adapter *Adapter, session sessionio.SessionRef) []sessionio.ReadItem {
	t.Helper()
	return collectOpened(t, func() (sessionio.Stream[sessionio.ReadItem], error) {
		return adapter.Read(context.Background(), session)
	})
}

func openItemStream(t *testing.T, adapter *Adapter, session sessionio.SessionRef) sessionio.Stream[sessionio.ReadItem] {
	t.Helper()
	stream, err := adapter.Read(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func collectOpened[T any](t *testing.T, open func() (sessionio.Stream[T], error)) []T {
	t.Helper()
	stream, err := open()
	if err != nil {
		t.Fatal(err)
	}
	return collectStream(t, stream)
}

func sessionByNativeID(t *testing.T, sessions []sessionio.SessionRef, nativeID string) sessionio.SessionRef {
	t.Helper()
	for _, session := range sessions {
		if session.NativeID == nativeID {
			return session
		}
	}
	t.Fatalf("session %q not found in %#v", nativeID, sessions)
	return sessionio.SessionRef{}
}

func collectStream[T any](t *testing.T, stream sessionio.Stream[T]) []T {
	t.Helper()
	defer stream.Close()
	var values []T
	for {
		value, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return values
		}
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
}

func hasDiagnostic(values []sessionio.Diagnostic, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}
