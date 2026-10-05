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
	"strings"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
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
		if err := sessionio.WriteJSON(io.Discard, sessionio.Producer{Name: "test", Version: "1"}, []sessionio.Record{{Kind: sessionio.RecordKindReadItem, ReadItem: &item}}); err != nil {
			t.Fatal(err)
		}
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
