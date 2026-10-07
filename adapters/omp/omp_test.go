package omp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

func writeFixture(t *testing.T, root, relative, data string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
func fixtureAdapter(t *testing.T, root string) *Adapter {
	t.Helper()
	config := DefaultConfig()
	config.AgentDir = root
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}
func drain[T any](t *testing.T, stream sessionio.Stream[T], err error) []T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var result []T
	for {
		value, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
}
func sessions(t *testing.T, adapter *Adapter) []sessionio.SessionRef {
	t.Helper()
	stream, err := adapter.Sessions(context.Background(), sessionio.SessionRequest{})
	return drain(t, stream, err)
}
func items(t *testing.T, adapter *Adapter, ref sessionio.SessionRef) []sessionio.ReadItem {
	t.Helper()
	stream, err := adapter.Read(context.Background(), ref)
	return drain(t, stream, err)
}
func requireEncodable(t *testing.T, item sessionio.ReadItem) {
	t.Helper()
	if err := sessionio.WriteJSON(io.Discard, sessionio.Producer{Name: "test", Version: "1"}, []sessionio.Record{{Kind: sessionio.RecordKindReadItem, ReadItem: &item}}); err != nil {
		t.Fatal(err)
	}
}

func TestTreeMessagesDatesAndExternalEvidence(t *testing.T) {
	root := t.TempDir()
	blob := []byte{0, 1, 2, 255}
	hash := fmt.Sprintf("%x", sha256.Sum256(blob))
	missing := strings.Repeat("a", 64)
	writeFixture(t, root, "blobs/"+hash, string(blob))
	body := strings.Join([]string{
		`{"type":"title","v":1,"title":"Current title","updatedAt":"2026-10-02T10:00:00Z","pad":""}`,
		`{"type":"session","version":3,"id":"tree","timestamp":"2026-10-01T10:00:00Z","cwd":"/workspace/a","title":"Old title"}`,
		`{"type":"message","id":"u","parentId":null,"timestamp":"2026-10-02T10:00:00Z","message":{"role":"user","timestamp":1790848800000,"content":"branch question"}}`,
		`{"type":"message","id":"abandoned","parentId":"u","timestamp":"2026-10-02T10:00:01Z","message":{"role":"assistant","timestamp":1790848860000,"content":[{"type":"text","text":"abandoned answer"}]}}`,
		fmt.Sprintf(`{"type":"message","id":"selected","parentId":"u","timestamp":"2026-10-02T10:00:02Z","message":{"role":"assistant","timestamp":1790848820000,"model":"test-model","provider":"test-provider","content":[{"type":"thinking","thinking":"reasoning"},{"type":"toolCall","id":"call","name":"read","arguments":{"path":"x"}},{"type":"image","mimeType":"image/png","data":"blob:sha256:%s"},{"type":"image","mimeType":"image/png","data":"blob:sha256:%s"}],"usage":{"input":2,"output":3,"cacheRead":4}}}`, hash, missing),
		`{"type":"message","id":"result","parentId":"selected","timestamp":"2026-10-03T10:00:00Z","message":{"role":"toolResult","toolCallId":"call","toolName":"read","timestamp":1791028800000,"isError":false,"content":[{"type":"text","text":"artifact://1"}]}}`,
		`{"type":"custom","id":"service","parentId":"result","timestamp":"2026-10-03T12:00:00Z","customType":"tool_execution_start","data":{"toolCallId":"other"}}`,
		`{"type":"compaction","id":"compact","parentId":"service","timestamp":"2026-10-03T13:00:00Z","summary":"retained summary"}`,
		`{"type":"reset_boundary","id":"reset","parentId":"compact","timestamp":"2026-10-03T14:00:00Z"}`,
	}, "\n") + "\n"
	writeFixture(t, root, "sessions/a/tree.jsonl", body)
	writeFixture(t, root, "sessions/a/tree/1.read.log", "full external output\n")
	writeFixture(t, root, "sessions/b/other.jsonl", `{"type":"session","version":2,"id":"other"}`+"\n")
	writeFixture(t, root, "sessions/a/tree/child.jsonl", `{"type":"session","version":3,"id":"child","timestamp":"2026-10-02T10:00:00Z"}`+"\n")
	adapter := fixtureAdapter(t, root)
	refs := sessions(t, adapter)
	if len(refs) != 3 {
		t.Fatalf("sessions = %d, want parent, child and other bucket", len(refs))
	}
	var ref sessionio.SessionRef
	for _, candidate := range refs {
		if candidate.NativeID == "tree" {
			ref = candidate
		}
		if candidate.NativeID == "other" && (candidate.CreatedAt != nil || candidate.LastMessageAt != nil) {
			t.Fatal("missing dates became known")
		}
	}
	if ref.Title != "Current title" || ref.CreatedAt == nil || ref.CreatedAt.Format(time.RFC3339) != "2026-10-01T10:00:00Z" || ref.LastMessageAt == nil || ref.LastMessageAt.UnixMilli() != 1790848860000 {
		t.Fatalf("metadata = %+v", ref)
	}
	read := items(t, adapter, ref)
	var reconstructed bytes.Buffer
	parents := map[string]string{}
	leaves := 0
	pairs := 0
	blobs := 0
	artifacts := 0
	reasoning := 0
	missingContent := 0
	for _, item := range read {
		if item.Observation.Locator.File.Path == "sessions/a/tree.jsonl" {
			reconstructed.Write(item.Observation.Representation.Data)
			reconstructed.Write(item.Observation.Representation.Framing)
		}
		if item.Observation.NativeKind == "blob" {
			blobs++
			if !bytes.Equal(item.Observation.Representation.Data, blob) {
				t.Fatal("blob changed")
			}
		}
		if item.Observation.NativeKind == "artifact" {
			artifacts++
			if string(item.Observation.Representation.Data) != "full external output\n" {
				t.Fatal("artifact changed")
			}
		}
		for _, relation := range item.Relations {
			if relation.Kind == sessionio.RelationKindReplyTo {
				parents[item.Observation.NativeKey] = relation.To.ID
			}
			if relation.Kind == sessionio.RelationKindActiveLeaf {
				leaves++
				if item.Observation.NativeKey != "reset" {
					t.Fatal("wrong active leaf")
				}
			}
			if relation.Kind == sessionio.RelationKindToolPair {
				pairs++
			}
		}
		for _, event := range item.Events {
			if event.Reasoning != nil {
				reasoning++
			}
			if item.Observation.NativeKey == "service" && event.Message != nil {
				t.Fatal("service flattened into message")
			}
			if event.Message != nil {
				for _, block := range event.Message.Content {
					if block.Availability == sessionio.ContentAvailabilityUnavailable {
						missingContent++
					}
				}
			}
		}
		requireEncodable(t, item)
	}
	if reconstructed.String() != body {
		t.Fatal("native transcript lost bytes")
	}
	if parents["abandoned"] == "" || parents["abandoned"] != parents["selected"] || leaves != 1 || pairs != 1 || blobs != 1 || artifacts != 1 || reasoning != 1 || missingContent != 1 {
		t.Fatalf("topology/fidelity = %v leaf=%d pair=%d blob=%d artifact=%d reasoning=%d missing=%d", parents, leaves, pairs, blobs, artifacts, reasoning, missingContent)
	}
	if err := os.Remove(filepath.Join(root, "blobs", hash)); err != nil {
		t.Fatal(err)
	}
	updated := sessions(t, adapter)
	for _, changed := range updated {
		if changed.NativeID == "tree" {
			if changed.DiscoveryRevision == ref.DiscoveryRevision {
				t.Fatal("missing blob did not invalidate freshness")
			}
			for _, item := range items(t, adapter, changed) {
				if item.Observation.NativeKind == "blob" {
					t.Fatal("missing blob fabricated")
				}
			}
		}
	}
}

func TestNamelessToolCallKeepsItsErrorResultPair(t *testing.T) {
	root := t.TempDir()
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"nameless"}`,
		`{"type":"message","id":"call","parentId":null,"message":{"role":"assistant","content":[{"type":"toolCall","id":"named","name":"read","arguments":{"path":"x"}},{"type":"toolCall","id":"blank","name":"","arguments":{}}]}}`,
		`{"type":"message","id":"result","parentId":"call","message":{"role":"toolResult","toolCallId":"blank","toolName":"","isError":true,"content":[{"type":"text","text":"Tool  not found"}]}}`,
	}, "\n") + "\n"
	writeFixture(t, root, "sessions/a/nameless.jsonl", body)
	adapter := fixtureAdapter(t, root)
	callEvents := map[string]sessionio.Event{}
	var result sessionio.Event
	var pairs []sessionio.Relation
	for _, item := range items(t, adapter, sessions(t, adapter)[0]) {
		requireEncodable(t, item)
		for _, event := range item.Events {
			if event.ToolCall != nil {
				callEvents[event.ToolCall.CallID] = event
			}
			if event.ToolResult != nil {
				result = event
			}
		}
		for _, relation := range item.Relations {
			if relation.Kind == sessionio.RelationKindToolPair {
				pairs = append(pairs, relation)
			}
		}
	}
	blank := callEvents["blank"]
	if len(callEvents) != 2 || callEvents["named"].ToolCall.Name != "read" || blank.ToolCall == nil || blank.ToolCall.Name != "" || string(blank.ToolCall.Input.Data) != "{}" {
		t.Fatalf("tool calls = %+v, want read and the nameless blank call with {} arguments", callEvents)
	}
	if result.ToolResult == nil || result.ToolResult.CallID != "blank" || result.ToolResult.Status != sessionio.ToolResultStatusError {
		t.Fatalf("tool result = %+v, want the blank call's error", result.ToolResult)
	}
	if len(pairs) != 1 || pairs[0].From.ID != string(blank.ID) || pairs[0].To.ID != string(result.ID) {
		t.Fatalf("tool pairs = %+v, want blank call %s paired with result %s", pairs, blank.ID, result.ID)
	}
}

func TestProseAgentMentionsAreNotFatalReferences(t *testing.T) {
	root := t.TempDir()
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"prose"}`,
		`{"type":"custom","id":"note","parentId":null,"customType":"x","data":{"text":"Do not read agent://... or agent://a..b; agent://Op... is gone, see agent://Real."}}`,
		`{"type":"custom","id":"invalid","parentId":"note","customType":"x","data":{"text":"agent://Op..bad"}}`,
		`{"type":"custom","id":"longer","parentId":"invalid","customType":"x","data":{"text":"agent://Opal artifact://12"}}`,
		`{"type":"custom","id":"artifact","parentId":"longer","customType":"x","data":{"text":"artifact://1"}}`,
	}, "\n") + "\n"
	writeFixture(t, root, "sessions/a/prose.jsonl", body)
	writeFixture(t, root, "sessions/a/prose/Real.md", "real output\n")
	adapter := fixtureAdapter(t, root)
	var outputs []string
	limitations := map[string][]sessionio.SourceLimitation{}
	for _, item := range items(t, adapter, sessions(t, adapter)[0]) {
		if item.Observation.NativeKind == "agent_output" {
			outputs = append(outputs, item.Observation.NativeKey+"="+string(item.Observation.Representation.Data))
		}
		limitations[item.Observation.NativeKey] = item.Observation.Limitations
	}
	if len(outputs) != 1 || outputs[0] != "agent://Real=real output\n" {
		t.Fatalf("agent outputs = %q, want only agent://Real", outputs)
	}
	want := map[string][]sessionio.SourceLimitation{
		"note": {
			{Kind: sessionio.LimitationKindMissingExternalPayload, Detail: "agent://Op"},
			{Kind: sessionio.LimitationKindExternalPayload, Detail: "agent://Real"},
		},
		"invalid": nil,
		"longer": {
			{Kind: sessionio.LimitationKindMissingExternalPayload, Detail: "agent://Opal"},
			{Kind: sessionio.LimitationKindMissingExternalPayload, Detail: "artifact://12"},
		},
		"artifact": {{Kind: sessionio.LimitationKindMissingExternalPayload, Detail: "artifact://1"}},
	}
	for key, expected := range want {
		if !reflect.DeepEqual(limitations[key], expected) {
			t.Fatalf("%s limitations = %+v, want %+v", key, limitations[key], expected)
		}
	}
}

func TestExternalPayloadsAboveRecordLimitBecomeLimitations(t *testing.T) {
	const limit = 512
	root := t.TempDir()
	blob := bytes.Repeat([]byte{7}, limit+1)
	// The oversized blob is never read, so its bytes need not match their content address.
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("other bytes")))
	writeFixture(t, root, "blobs/"+hash, string(blob))
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"large"}`,
		`{"type":"custom","id":"below","parentId":null,"customType":"x","data":{"text":"artifact://1"}}`,
		`{"type":"custom","id":"equal","parentId":"below","customType":"x","data":{"text":"artifact://2"}}`,
		`{"type":"custom","id":"above","parentId":"equal","customType":"x","data":{"text":"artifact://3"}}`,
		fmt.Sprintf(`{"type":"message","id":"image","parentId":"above","message":{"role":"assistant","content":[{"type":"image","mimeType":"image/png","data":"blob:sha256:%s"}]}}`, hash),
	}, "\n") + "\n"
	writeFixture(t, root, "sessions/a/large.jsonl", body)
	payloads := map[string]string{
		"artifact://1": strings.Repeat("b", limit-1),
		"artifact://2": strings.Repeat("e", limit),
		"artifact://3": strings.Repeat("a", limit+1),
	}
	for index := 1; index <= 3; index++ {
		writeFixture(t, root, fmt.Sprintf("sessions/a/large/%d.bash.log", index), payloads[fmt.Sprintf("artifact://%d", index)])
	}
	config := DefaultConfig()
	config.AgentDir = root
	config.MaxRecordBytes = limit
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	refs := sessions(t, adapter)
	if len(refs) != 1 {
		t.Fatalf("sessions = %d, want 1", len(refs))
	}
	emitted := map[string]string{}
	limitations := map[string][]sessionio.SourceLimitation{}
	imageAvailability := sessionio.ContentAvailability("")
	for _, item := range items(t, adapter, refs[0]) {
		switch item.Observation.NativeKind {
		case "artifact", "blob":
			emitted[item.Observation.NativeKey] = string(item.Observation.Representation.Data)
		default:
			limitations[item.Observation.NativeKey] = item.Observation.Limitations
		}
		if item.Observation.NativeKey == "image" {
			imageAvailability = item.Events[0].Message.Content[0].Availability
		}
		requireEncodable(t, item)
	}
	if len(emitted) != 2 || emitted["artifact://1"] != payloads["artifact://1"] || emitted["artifact://2"] != payloads["artifact://2"] {
		t.Fatalf("emitted payloads = %v, want only the two within the limit", emitted)
	}
	want := map[string]sessionio.SourceLimitation{
		"below": {Kind: sessionio.LimitationKindExternalPayload, Detail: "artifact://1"},
		"equal": {Kind: sessionio.LimitationKindExternalPayload, Detail: "artifact://2"},
		"above": {Kind: sessionio.LimitationKindOversizedExternalPayload, Detail: "artifact://3"},
		"image": {Kind: sessionio.LimitationKindOversizedExternalPayload, Detail: "blob:sha256:" + hash},
	}
	for key, limitation := range want {
		if got := limitations[key]; len(got) != 1 || got[0] != limitation {
			t.Fatalf("%s limitations = %+v, want %+v", key, got, limitation)
		}
	}
	if imageAvailability != sessionio.ContentAvailabilityUnavailable {
		t.Fatalf("oversized image availability = %q, want unavailable", imageAvailability)
	}

	unlimitedRoot := t.TempDir()
	writeFixture(t, unlimitedRoot, "sessions/a/large.jsonl", body)
	writeFixture(t, unlimitedRoot, "sessions/a/large/3.bash.log", payloads["artifact://3"])
	config.AgentDir = unlimitedRoot
	config.MaxRecordBytes = sourceio.UnlimitedRecordBytes
	unlimited, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items(t, unlimited, sessions(t, unlimited)[0]) {
		found = found || (item.Observation.NativeKey == "artifact://3" && string(item.Observation.Representation.Data) == payloads["artifact://3"])
	}
	if !found {
		t.Fatal("unlimited mode did not emit the payload above the finite limit")
	}
}

func TestHistoricalDatesAndMalformedBoundaries(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			root := t.TempDir()
			body := fmt.Sprintf(`{"type":"session","version":%d,"id":"history","timestamp":"invalid"}`+"\n", version) + `{"type":"message","id":"first","parentId":null,"timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","timestamp":1790848800000,"content":"valid"}}` + "\n" + `{"type":"message","id":"second","parentId":"first","timestamp":"2026-10-02T10:00:00Z","message":{"role":"assistant","timestamp":"1790935200000","content":[]}}` + "\n"
			writeFixture(t, root, "sessions/history/file.jsonl", body)
			adapter := fixtureAdapter(t, root)
			ref := sessions(t, adapter)[0]
			if ref.CreatedAt != nil || ref.LastMessageAt == nil || ref.LastMessageAt.UnixMilli() != 1790848800000 || len(ref.Diagnostics) != 2 {
				t.Fatalf("dates = %+v", ref)
			}
			for _, d := range ref.Diagnostics {
				if d.Cause == nil || d.Locator == nil || d.Locator.File.Record == nil {
					t.Fatal("invalid timestamp lacks source context")
				}
			}
			read := items(t, adapter, ref)
			if read[len(read)-1].Observation.NativeKey != "second" {
				t.Fatal("raw invalid date record lost")
			}
		})
	}
	for _, body := range []string{`{"type":"session","version":4,"id":"future"}` + "\n", `{"type":"session","version":3,"id":"bad"}` + "\nnot-json\n", `{"type":"message","message":{"role":"user"}}` + "\n"} {
		root := t.TempDir()
		writeFixture(t, root, "sessions/a/bad.jsonl", body)
		_, err := fixtureAdapter(t, root).Sessions(context.Background(), sessionio.SessionRequest{})
		var located *sessionio.ReaderError
		if !errors.As(err, &located) || located.Locator == nil {
			t.Fatalf("unlocated malformed source: %v", err)
		}
	}
}

func TestGrowingTailAndDuplicateParents(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"session","version":3,"id":"pending"}` + "\n" + `{"type":"custom","id":"same","parentId":null,"customType":"a"}` + "\n" + `{"type":"custom","id":"same","parentId":null,"customType":"b"}` + "\n" + `{"type":"custom","id":"child","parentId":"same","customType":"c"}` + "\n" + `{"type":"message","id":"pending"`
	writeFixture(t, root, "sessions/a/pending.jsonl", body)
	adapter := fixtureAdapter(t, root)
	read := items(t, adapter, sessions(t, adapter)[0])
	if len(read) != 4 {
		t.Fatalf("pending tail emitted: %d", len(read))
	}
	last := read[3]
	for _, r := range last.Relations {
		if r.Kind == sessionio.RelationKindReplyTo {
			t.Fatal("ambiguous parent resolved")
		}
	}
	if len(last.Diagnostics) != 1 {
		t.Fatal("missing parent diagnostic")
	}
}
