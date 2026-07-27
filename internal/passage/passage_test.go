package passage

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func textBlock(text string) sessionio.ContentBlock {
	return sessionio.ContentBlock{
		Kind:         sessionio.ContentKindText,
		Availability: sessionio.ContentAvailabilityAvailable,
		Text:         &sessionio.TextContent{Text: text},
	}
}

func message(
	id string,
	role sessionio.MessageRole,
	text string,
) sessionio.Event {
	return sessionio.Event{
		ID:   sessionio.EventID(id),
		Kind: sessionio.EventKindMessage,
		Evidence: []sessionio.EvidenceRef{{
			Observation: sessionio.ObservationID("observation-" + id),
			Locator: sessionio.SourceLocator{
				Kind: sessionio.LocatorKindFile,
				File: &sessionio.FileLocator{Root: "/root", Path: "session.jsonl"},
			},
		}},
		Message: &sessionio.MessageEvent{
			Role:    role,
			Content: []sessionio.ContentBlock{textBlock(text)},
		},
	}
}

func item(events ...sessionio.Event) sessionio.ReadItem {
	return sessionio.ReadItem{
		Observation: sessionio.NativeObservation{ID: "observation"},
		Events:      events,
	}
}

func TestContiguousAssistantMessagesFormOnePassage(t *testing.T) {
	built := Build([]sessionio.ReadItem{
		item(message("u1", sessionio.MessageRoleUser, "first question")),
		item(
			message("a1", sessionio.MessageRoleAssistant, "first half"),
			message("a2", sessionio.MessageRoleAssistant, "second half"),
		),
		item(message("u2", sessionio.MessageRoleUser, "second question")),
	})
	if len(built.Passages) != 3 {
		t.Fatalf("passages = %d, want 3: %+v", len(built.Passages), built.Passages)
	}
	if built.Passages[1].Kind != KindAssistant {
		t.Fatalf("second passage kind = %q, want %q",
			built.Passages[1].Kind, KindAssistant)
	}
	if len(built.Passages[1].Events) != 2 {
		t.Fatalf("assistant passage events = %d, want 2",
			len(built.Passages[1].Events))
	}
	if built.Passages[1].Body != "first half\nsecond half" {
		t.Fatalf("assistant body = %q, want the joined halves",
			built.Passages[1].Body)
	}
	if built.Passages[2].Kind != KindUser {
		t.Fatalf("third passage kind = %q, want %q",
			built.Passages[2].Kind, KindUser)
	}
}

// A user message must break an assistant run even when more assistant content
// follows, otherwise a passage would span a turn boundary.
func TestUserMessageClosesTheAssistantPassage(t *testing.T) {
	built := Build([]sessionio.ReadItem{item(
		message("a1", sessionio.MessageRoleAssistant, "before"),
		message("u1", sessionio.MessageRoleUser, "interrupt"),
		message("a2", sessionio.MessageRoleAssistant, "after"),
	)})
	kinds := []Kind{KindAssistant, KindUser, KindAssistant}
	if len(built.Passages) != len(kinds) {
		t.Fatalf("passages = %d, want %d", len(built.Passages), len(kinds))
	}
	for index, want := range kinds {
		if built.Passages[index].Kind != want {
			t.Fatalf("passage %d kind = %q, want %q",
				index, built.Passages[index].Kind, want)
		}
	}
}

func TestToolCallAndResultStayDistinctPassages(t *testing.T) {
	built := Build([]sessionio.ReadItem{item(
		sessionio.Event{
			ID:   "c1",
			Kind: sessionio.EventKindToolCall,
			ToolCall: &sessionio.ToolCallEvent{
				CallID: "call-1",
				Name:   "shell",
				Input: sessionio.Payload{
					MediaType: "application/json",
					Data:      []byte(`{"command":"ls"}`),
				},
			},
		},
		sessionio.Event{
			ID:   "r1",
			Kind: sessionio.EventKindToolResult,
			ToolResult: &sessionio.ToolResultEvent{
				CallID: "call-1",
				Status: sessionio.ToolResultStatusError,
				Output: sessionio.Payload{
					MediaType: "text/plain",
					Data:      []byte("ECONNRESET: socket hang up"),
				},
			},
		},
	)})
	if len(built.Passages) != 2 {
		t.Fatalf("passages = %d, want 2", len(built.Passages))
	}
	if built.Passages[0].Kind != KindToolCall ||
		built.Passages[1].Kind != KindToolResult {
		t.Fatalf("passage kinds = %q, %q, want tool_call, tool_result",
			built.Passages[0].Kind, built.Passages[1].Kind)
	}
	if built.Passages[0].Body != "shell\n{\"command\":\"ls\"}" {
		t.Fatalf("tool call body = %q", built.Passages[0].Body)
	}
	if built.Passages[1].Body != "ECONNRESET: socket hang up" {
		t.Fatalf("tool result body = %q", built.Passages[1].Body)
	}
}

func toolResult(data []byte) sessionio.ReadItem {
	return item(sessionio.Event{
		ID:   "r1",
		Kind: sessionio.EventKindToolResult,
		ToolResult: &sessionio.ToolResultEvent{
			CallID: "call-1",
			Status: sessionio.ToolResultStatusSuccess,
			Output: sessionio.Payload{MediaType: "text/plain", Data: data},
		},
	})
}

// Binary payloads stay in the retained native record; a lossy decode would put
// replacement characters into the searchable projection.
func TestBinaryToolPayloadProducesNoPassage(t *testing.T) {
	built := Build([]sessionio.ReadItem{
		toolResult([]byte{0xff, 0xfe, 0x00, 0x01}),
	})
	if len(built.Events) != 1 {
		t.Fatalf("events = %d, want the retained event", len(built.Events))
	}
	if len(built.Passages) != 0 {
		t.Fatalf("passages = %d, want none: %+v",
			len(built.Passages), built.Passages)
	}
}

// Real transcripts carry NUL inside tool output; no PostgreSQL text column can
// store it. Every other control character stays, and the loss is reported.
func TestNulBytesLeaveTheProjectionAsATypedLimitation(t *testing.T) {
	built := Build([]sessionio.ReadItem{
		toolResult([]byte("before\x00mid\x07dle\x00after")),
	})
	if len(built.Passages) != 1 {
		t.Fatalf("passages = %d, want 1", len(built.Passages))
	}
	if built.Passages[0].Body != "beforemid\x07dleafter" {
		t.Fatalf("body = %q, want the NUL removed and the BEL kept",
			built.Passages[0].Body)
	}
	want := []Limitation{{Kind: LimitationNULRemoved, RemovedBytes: 2}}
	if !reflect.DeepEqual(built.Passages[0].Limitations, want) {
		t.Fatalf("limitations = %+v, want %+v",
			built.Passages[0].Limitations, want)
	}
}

func TestProjectionWithoutNulCarriesNoLimitation(t *testing.T) {
	built := Build([]sessionio.ReadItem{toolResult([]byte("clean\x1boutput"))})
	if len(built.Passages) != 1 {
		t.Fatalf("passages = %d, want 1", len(built.Passages))
	}
	if built.Passages[0].Body != "clean\x1boutput" {
		t.Fatalf("body = %q, want the escape byte untouched",
			built.Passages[0].Body)
	}
	if len(built.Passages[0].Limitations) != 0 {
		t.Fatalf("limitations = %+v, want none", built.Passages[0].Limitations)
	}
}

func TestUnavailableContentIsNotProjected(t *testing.T) {
	redacted := sessionio.ContentBlock{
		Kind:         sessionio.ContentKindText,
		Availability: sessionio.ContentAvailabilityRedacted,
		Text:         &sessionio.TextContent{Text: "secret"},
	}
	built := Build([]sessionio.ReadItem{item(sessionio.Event{
		ID:   "u1",
		Kind: sessionio.EventKindMessage,
		Message: &sessionio.MessageEvent{
			Role:    sessionio.MessageRoleUser,
			Content: []sessionio.ContentBlock{redacted, textBlock("visible")},
		},
	})})
	if len(built.Passages) != 1 {
		t.Fatalf("passages = %d, want 1", len(built.Passages))
	}
	if built.Passages[0].Body != "visible" {
		t.Fatalf("body = %q, want only the available block",
			built.Passages[0].Body)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	moment := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	events := []sessionio.Event{
		message("u1", sessionio.MessageRoleUser, "same input"),
		message("a1", sessionio.MessageRoleAssistant, "same answer"),
	}
	events[0].Timestamp = &moment
	first := Build([]sessionio.ReadItem{item(events...)})
	second := Build([]sessionio.ReadItem{item(events...)})
	if len(first.Passages) != len(second.Passages) {
		t.Fatalf("passage counts differ: %d and %d",
			len(first.Passages), len(second.Passages))
	}
	for index := range first.Passages {
		if first.Passages[index].Body != second.Passages[index].Body {
			t.Fatalf("passage %d body differs", index)
		}
		if !bytes.Equal(
			first.Passages[index].ContentHash,
			second.Passages[index].ContentHash,
		) {
			t.Fatalf("passage %d content hash differs", index)
		}
	}
	if bytes.Equal(first.Passages[0].ContentHash, first.Passages[1].ContentHash) {
		t.Fatal("different passages share a content hash")
	}
}

func TestEventsCarryEvidenceAndObservation(t *testing.T) {
	built := Build([]sessionio.ReadItem{item(
		message("u1", sessionio.MessageRoleUser, "question"),
	)})
	if len(built.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(built.Events))
	}
	event := built.Events[0]
	if event.Observation != "observation" {
		t.Fatalf("observation = %q, want the item observation", event.Observation)
	}
	if len(event.Evidence) != 1 {
		t.Fatalf("evidence = %d, want 1", len(event.Evidence))
	}
	if event.Evidence[0].Observation != "observation-u1" {
		t.Fatalf("evidence observation = %q", event.Evidence[0].Observation)
	}
	if event.Role != string(sessionio.MessageRoleUser) {
		t.Fatalf("role = %q, want user", event.Role)
	}
}

func TestDisplayCollapsesAndBounds(t *testing.T) {
	if got := Display("  a\n\tb  c  ", 0); got != "a b c" {
		t.Fatalf("display = %q, want the collapsed body", got)
	}
	if got := Display("abcdefgh", 4); got != "abcd..." {
		t.Fatalf("display = %q, want a bounded excerpt", got)
	}
}

func reasoning(id string, content string, summary string) sessionio.Event {
	event := sessionio.Event{
		ID:        sessionio.EventID(id),
		Kind:      sessionio.EventKindReasoning,
		Reasoning: &sessionio.ReasoningEvent{},
	}
	if content != "" {
		event.Reasoning.Content = []sessionio.ContentBlock{textBlock(content)}
	}
	if summary != "" {
		event.Reasoning.Summary = []sessionio.ContentBlock{textBlock(summary)}
	}
	return event
}

func TestReasoningSummaryStaysDistinctContent(t *testing.T) {
	built := Build([]sessionio.ReadItem{
		item(reasoning("r1", "long chain of thought", "short summary")),
	})
	if len(built.Passages) != 2 {
		t.Fatalf("passages = %+v, want reasoning and its summary", built.Passages)
	}
	if built.Passages[0].Kind != KindReasoning ||
		built.Passages[0].Body != "long chain of thought" {
		t.Fatalf("reasoning passage = %+v", built.Passages[0])
	}
	if built.Passages[1].Kind != KindReasoningSummary ||
		built.Passages[1].Body != "short summary" {
		t.Fatalf("summary passage = %+v", built.Passages[1])
	}
	if bytes.Equal(built.Passages[0].ContentHash, built.Passages[1].ContentHash) {
		t.Fatal("reasoning and its summary share one content hash")
	}
}

func TestReasoningWithoutSummaryStaysOnePassage(t *testing.T) {
	built := Build([]sessionio.ReadItem{item(reasoning("r1", "thought", ""))})
	if len(built.Passages) != 1 || built.Passages[0].Kind != KindReasoning {
		t.Fatalf("passages = %+v", built.Passages)
	}
}

func TestOversizedToolResultSplitsOnLineBoundaries(t *testing.T) {
	line := strings.Repeat("d", 1023) + "\n"
	body := strings.Repeat(line, (MaxBodyBytes/len(line))+8)
	built := Build([]sessionio.ReadItem{toolResult([]byte(body))})
	if len(built.Passages) < 2 {
		t.Fatalf("passages = %d, want a split body", len(built.Passages))
	}
	var joined strings.Builder
	for index, passage := range built.Passages {
		if passage.Kind != KindToolResult {
			t.Fatalf("part %d kind = %q", index, passage.Kind)
		}
		if passage.Part != index || passage.Parts != len(built.Passages) {
			t.Fatalf("part %d = %d/%d", index, passage.Part, passage.Parts)
		}
		if len(passage.Body) > MaxBodyBytes {
			t.Fatalf("part %d is %d bytes", index, len(passage.Body))
		}
		if strings.Count(passage.Body, "\n") == 0 {
			t.Fatalf("part %d did not split on a line boundary", index)
		}
		joined.WriteString(passage.Body)
	}
	if joined.String() != body {
		t.Fatal("split parts do not reproduce the projected body")
	}
}

// A structurally indivisible span is the only case that falls back to a window,
// and the window must never cut a rune in half.
func TestIndivisibleSpanSplitsOnRuneBoundaries(t *testing.T) {
	body := strings.Repeat("я", MaxBodyBytes)
	built := Build([]sessionio.ReadItem{toolResult([]byte(body))})
	if len(built.Passages) < 2 {
		t.Fatalf("passages = %d, want a windowed body", len(built.Passages))
	}
	var joined strings.Builder
	for index, passage := range built.Passages {
		if !utf8.ValidString(passage.Body) {
			t.Fatalf("part %d cut a rune in half", index)
		}
		joined.WriteString(passage.Body)
	}
	if joined.String() != body {
		t.Fatal("windowed parts do not reproduce the projected body")
	}
}

func TestSplitBodyReportsRemovedNULOnce(t *testing.T) {
	line := strings.Repeat("e", 1023) + "\n"
	body := "\x00" + strings.Repeat(line, (MaxBodyBytes/len(line))+4)
	built := Build([]sessionio.ReadItem{toolResult([]byte(body))})
	var limitations int
	for _, passage := range built.Passages {
		limitations += len(passage.Limitations)
	}
	if limitations != 1 || len(built.Passages[0].Limitations) != 1 ||
		built.Passages[0].Limitations[0].RemovedBytes != 1 {
		t.Fatalf("limitations = %+v", built.Passages)
	}
}

func TestRelationsAreRetainedInSourceOrder(t *testing.T) {
	first := sessionio.Relation{
		ID:     "relation-1",
		Kind:   sessionio.RelationKindReplyTo,
		From:   sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: "b"},
		To:     sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: "a"},
		Origin: sessionio.RelationOriginNative,
		Evidence: []sessionio.EvidenceRef{{
			Observation: sessionio.ObservationID("observation-b"),
		}},
	}
	second := sessionio.Relation{
		ID:     "relation-2",
		Kind:   sessionio.RelationKindBranchParent,
		From:   sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: "session"},
		To:     sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: "peer"},
		Origin: sessionio.RelationOriginNative,
	}
	items := []sessionio.ReadItem{
		item(message("u1", sessionio.MessageRoleUser, "one")),
		item(message("u2", sessionio.MessageRoleUser, "two")),
	}
	items[0].Relations = []sessionio.Relation{first}
	items[1].Relations = []sessionio.Relation{second}
	built := Build(items)
	want := []Relation{
		{
			Kind:        string(sessionio.RelationKindReplyTo),
			Origin:      string(sessionio.RelationOriginNative),
			FromKind:    string(sessionio.NodeKindObservation),
			FromRef:     "b",
			ToKind:      string(sessionio.NodeKindObservation),
			ToRef:       "a",
			Observation: "observation-b",
		},
		{
			Kind:     string(sessionio.RelationKindBranchParent),
			Origin:   string(sessionio.RelationOriginNative),
			FromKind: string(sessionio.NodeKindSession),
			FromRef:  "session",
			ToKind:   string(sessionio.NodeKindObservation),
			ToRef:    "peer",
		},
	}
	if !reflect.DeepEqual(built.Relations, want) {
		t.Fatalf("relations = %+v, want %+v", built.Relations, want)
	}
}
