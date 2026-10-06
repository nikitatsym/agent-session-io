package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nikitatsym/agent-session-io"
)

func TestCompletedRolloutProjection(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
			home := fixtureHome(t)
			if compressed {
				name := "rollout-2026-07-24T18-00-00-10000000-0000-4000-8000-000000000018.jsonl"
				data, err := os.ReadFile(filepath.Join(home, "sessions", "2026", "07", "24", name))
				if err != nil {
					t.Fatal(err)
				}
				home = t.TempDir()
				writeCompressedRollout(t, home, false, name+".zst", data)
			}
			adapter := newFixtureAdapter(t, home)
			session := sessionByIdentity(t, collectSessions(t, adapter), "session-rich")
			items := collectReadItems(t, adapter, session)
			if session.LastMessageAt == nil || session.LastMessageAt.Format(time.RFC3339) != "2026-07-24T18:00:25Z" {
				t.Fatalf("last message = %v", session.LastMessageAt)
			}
			for _, item := range items[12:] {
				event := item.Events[0]
				if len(event.Evidence) != 1 || event.Evidence[0].Observation != item.Observation.ID ||
					!reflect.DeepEqual(event.Evidence[0].Locator, item.Observation.Locator) ||
					!reflect.DeepEqual(event.Timestamp, item.Observation.Timestamp) {
					t.Fatalf("projection provenance = %#v", event)
				}
			}
			assertFact(t, items[12], sessionio.FactKindModel, "example-model")
			assertFact(t, items[12], sessionio.FactKindWorkingDirectory, "/work/example")
			assertFact(t, items[12], sessionio.FactKindCurrentDate, "2026-07-24")
			assertFact(t, items[12], sessionio.FactKindTimezone, "UTC")
			assertFact(t, items[13], sessionio.FactKindWorkingDirectory, "/work/example/next")
			if len(items[13].Events[0].Facts.Facts) != 1 {
				t.Fatalf("delta replays state = %#v", items[13].Events)
			}
			assertMarker(t, items[14], "world_state", "delta")
			assertMarker(t, items[41], "thread_settings_applied", "")
			assertMarker(t, items[42], "world_state", "full")
			assertFact(t, items[15], sessionio.FactKindProvider, "example-provider")
			assertFact(t, items[15], sessionio.FactKindModel, "example-model")
			assertFact(t, items[15], sessionio.FactKindWorkingDirectory, "/work/example")
			assertFact(t, items[15], sessionio.FactKindApprovalPolicy, "on-request")
			if len(items[15].Events[0].Facts.Facts) != 4 {
				t.Fatalf("null settings invented facts = %#v", items[15].Events)
			}
			assertFact(t, items[40], sessionio.FactKindEffort, "high")
			for _, index := range []int{16, 18} {
				usage := items[index].Events[0].Usage
				input, output, total, cacheRead, cacheWrite, reasoning := int64(3), int64(2), int64(5), int64(0), int64(1), int64(1)
				if index == 18 {
					input, output, total, cacheRead, cacheWrite, reasoning = 5, 3, 8, 1, 2, 2
				}
				expected := &sessionio.UsageEvent{InputTokens: &input, OutputTokens: &output, TotalTokens: &total, CacheReadTokens: &cacheRead, CacheWriteTokens: &cacheWrite, ReasoningTokens: &reasoning}
				if !reflect.DeepEqual(usage, expected) || !reflect.DeepEqual(usage, items[index+1].Events[0].Usage) {
					t.Fatalf("cumulative usage = %#v, token_count = %#v", usage, items[index+1].Events[0].Usage)
				}
			}
			for _, indexes := range [][]int{{20, 21, 22}, {23, 24}} {
				first := items[indexes[0]]
				for _, index := range indexes[1:] {
					mirror := items[index]
					if first.Observation.ID == mirror.Observation.ID || first.Events[0].ID == mirror.Events[0].ID ||
						first.Events[0].Message.Content[0].Text.Text != mirror.Events[0].Message.Content[0].Text.Text ||
						first.Events[0].Message.Role != mirror.Events[0].Message.Role || len(mirror.Relations) != 0 {
						t.Fatalf("mirror observations lost distinction: %#v %#v", first, mirror)
					}
				}
			}
			async := items[25].Events[0].Message
			if async == nil || async.Role != sessionio.MessageRoleAssistant || len(async.Content) != 2 ||
				async.Content[0].Text.Text != "question" || async.Content[1].Opaque.NativeType != "FutureBlock" {
				t.Fatalf("async question = %#v", async)
			}
			assertMarker(t, items[26], "item_completed", "Reasoning")
			reasoning := items[27].Events[0].Reasoning
			if reasoning == nil || len(reasoning.Content) != 2 || len(reasoning.Summary) != 1 ||
				reasoning.Content[0].Text.Text != "detail" || reasoning.Content[1].Text.Text != "next detail" || reasoning.Summary[0].Text.Text != "summary" {
				t.Fatalf("reasoning text = %#v", reasoning)
			}
			statuses := []sessionio.ToolResultStatus{
				sessionio.ToolResultStatusSuccess, sessionio.ToolResultStatusError,
				sessionio.ToolResultStatusSuccess, sessionio.ToolResultStatusError,
				sessionio.ToolResultStatusSuccess, sessionio.ToolResultStatusError,
				sessionio.ToolResultStatusUnknown, sessionio.ToolResultStatusUnknown,
				sessionio.ToolResultStatusUnknown, sessionio.ToolResultStatusUnknown,
			}
			for offset, status := range statuses {
				item := items[28+offset]
				result := item.Events[0].ToolResult
				var record struct {
					Payload struct {
						Item json.RawMessage `json:"item"`
					} `json:"payload"`
				}
				if err := json.Unmarshal(item.Observation.Representation.Data, &record); err != nil {
					t.Fatal(err)
				}
				var native struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(record.Payload.Item, &native); err != nil {
					t.Fatal(err)
				}
				if result == nil || result.CallID != native.ID || result.Status != status ||
					result.Output.MediaType != "application/json" || string(result.Output.Data) != string(record.Payload.Item) ||
					len(item.Relations) != 0 || item.Events[0].ToolCall != nil {
					t.Fatalf("completion tool = %#v relations=%#v", result, item.Relations)
				}
			}
			for offset, subtype := range []string{"item_completed.FutureItem", "item_completed.Extension.future.extension"} {
				item := items[38+offset]
				if item.Events[0].Unknown == nil || item.Events[0].Unknown.NativeType != subtype ||
					len(item.Diagnostics) != 1 || item.Diagnostics[0].Code != "codex_unknown_item_subtype" ||
					!reflect.DeepEqual(item.Diagnostics[0].Locator, &item.Observation.Locator) {
					t.Fatalf("future subtype = %#v", item)
				}
			}
		})
	}
}

func assertMarker(t *testing.T, item sessionio.ReadItem, name, state string) {
	t.Helper()
	marker := item.Events[0].Marker
	if marker == nil || marker.Name != name || marker.State != state {
		t.Fatalf("marker = %#v, want %s/%s", marker, name, state)
	}
}

func TestCompletionResultsRespectPairCardinality(t *testing.T) {
	for _, subtype := range []string{"CommandExecution", "FileChange", "McpToolCall", "Extension"} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%t", subtype, direct), func(t *testing.T) {
				home := t.TempDir()
				item := `{"type":"` + subtype + `","kind":"web.search","id":"shared","status":"completed","result":{"isError":false}}`
				completion := `{"type":"event_msg","payload":{"type":"item_completed","item":` + item + `}}`
				if direct {
					completion = `{"type":"item_completed","item":` + item + `}`
				}
				data := `{"id":"10000000-0000-4000-8000-000000000099","type":"session_meta"}` + "\n" +
					`{"type":"function_call","call_id":"shared","name":"example","arguments":"{}"}` + "\n" +
					`{"type":"function_call_output","call_id":"shared","output":"example"}` + "\n" + completion + "\n"
				writeRollout(t, home, false, "rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000099.jsonl", []byte(data))
				adapter := newFixtureAdapter(t, home)
				for _, item := range collectReadItems(t, adapter, collectSessions(t, adapter)[0]) {
					if len(item.Relations) != 0 {
						t.Fatalf("duplicate result guessed a pair: %#v", item.Relations)
					}
				}
			})
		}
	}
}

func TestThreadUsagePreservesAbsentCounters(t *testing.T) {
	home := t.TempDir()
	data := `{"id":"10000000-0000-4000-8000-000000000099","type":"session_meta"}` + "\n" +
		`{"type":"token_usage_record","payload":{"thread_token_usage":{"cache_write_input_tokens":0}}}` + "\n"
	writeRollout(t, home, false, "rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000099.jsonl", []byte(data))
	adapter := newFixtureAdapter(t, home)
	items := collectReadItems(t, adapter, collectSessions(t, adapter)[0])
	usage := items[1].Events[0].Usage
	zero := int64(0)
	if !reflect.DeepEqual(usage, &sessionio.UsageEvent{CacheWriteTokens: &zero}) {
		t.Fatalf("absent counters became zero = %#v", usage)
	}
}

func TestFutureItemShapesRemainNativeUnknown(t *testing.T) {
	home := t.TempDir()
	data := `{"id":"10000000-0000-4000-8000-000000000099","type":"session_meta"}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FutureItem","content":1,"raw_content":1,"status":1,"result":1}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Extension","kind":"future.extension","content":1,"status":1,"result":1}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"type":"FutureBlock","text":{"native":1}}]}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","raw_content":[""],"summary_text":[""]}}}` + "\n"
	writeRollout(t, home, false, "rollout-2026-07-24T10-00-00-10000000-0000-4000-8000-000000000099.jsonl", []byte(data))
	adapter := newFixtureAdapter(t, home)
	items := collectReadItems(t, adapter, collectSessions(t, adapter)[0])
	for index, subtype := range []string{"item_completed.FutureItem", "item_completed.Extension.future.extension"} {
		item := items[index+1]
		if item.Events[0].Unknown == nil || item.Events[0].Unknown.NativeType != subtype ||
			len(item.Diagnostics) != 1 || item.Diagnostics[0].Code != "codex_unknown_item_subtype" {
			t.Fatalf("future shape was interpreted = %#v", item)
		}
	}
	content := items[3].Events[0].Message.Content[0]
	if content.Opaque == nil || string(content.Opaque.Data) != `{"type":"FutureBlock","text":{"native":1}}` {
		t.Fatalf("future block lost native shape = %#v", content)
	}
	assertMarker(t, items[4], "item_completed", "Reasoning")
}
