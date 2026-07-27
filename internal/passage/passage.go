// Package passage turns normalized reader items into deterministic passages
// and their searchable projections.
package passage

import (
	"crypto/sha256"
	"strings"
	"time"
	"unicode/utf8"

	sessionio "github.com/nikitatsym/agent-session-io"
)

// BuilderVersion identifies this passage builder in every stored passage row.
const BuilderVersion = "sessionio.passage/v1"

// ProjectionVersion identifies the projection builder in every stored
// projection row. It changes independently of BuilderVersion.
const ProjectionVersion = "sessionio.projection/v1"

// Kind is the structural class of one passage.
type Kind string

const (
	KindUser       Kind = "user"
	KindAssistant  Kind = "assistant"
	KindSystem     Kind = "system"
	KindReasoning  Kind = "reasoning"
	KindToolCall   Kind = "tool_call"
	KindToolResult Kind = "tool_result"
)

// LimitationNULRemoved names the projection whose body lost U+0000. It is the
// only projection limitation this builder can produce: no other control
// character is altered.
const LimitationNULRemoved = "nul_removed"

// Limitation reports that a projection is not byte-exact to the native content
// it was built from.
type Limitation struct {
	Kind         string
	RemovedBytes int64
}

// Evidence points one event at the native observation that produced it.
type Evidence struct {
	Observation string
	Locator     sessionio.SourceLocator
}

// Event is the retained normalized event behind one or more passages.
type Event struct {
	Key         string
	Kind        string
	Role        string
	Observation string
	OccurredAt  *time.Time
	Evidence    []Evidence
	// Text is the searchable projection input; empty for non-text events.
	Text string
	// RemovedNUL counts the U+0000 bytes dropped from Text.
	RemovedNUL int64
}

// Passage groups contiguous events of one structural class.
type Passage struct {
	Kind Kind
	// Events indexes into Session.Events in source order.
	Events      []int
	Body        string
	ContentHash []byte
	OccurredAt  *time.Time
	Limitations []Limitation
}

// Session carries every retained event and passage of one reader session.
type Session struct {
	Events   []Event
	Passages []Passage
}

// Build is deterministic: the same reader items always produce the same
// passages, bodies, and content hashes.
func Build(items []sessionio.ReadItem) Session {
	var session Session
	openAssistant := -1
	for itemIndex := range items {
		for eventIndex := range items[itemIndex].Events {
			event := buildEvent(
				items[itemIndex].Observation,
				items[itemIndex].Events[eventIndex],
			)
			index := len(session.Events)
			session.Events = append(session.Events, event)
			if event.Text == "" {
				continue
			}
			kind := passageKind(event)
			if kind == KindAssistant && openAssistant >= 0 {
				session.Passages[openAssistant].Events = append(
					session.Passages[openAssistant].Events,
					index,
				)
				continue
			}
			session.Passages = append(session.Passages, Passage{
				Kind:       kind,
				Events:     []int{index},
				OccurredAt: event.OccurredAt,
			})
			if kind == KindAssistant {
				openAssistant = len(session.Passages) - 1
				continue
			}
			openAssistant = -1
		}
	}
	for index := range session.Passages {
		body := passageBody(session, session.Passages[index])
		session.Passages[index].Body = body
		session.Passages[index].ContentHash = contentHash(
			session.Passages[index].Kind,
			body,
		)
		session.Passages[index].Limitations = passageLimitations(
			session,
			session.Passages[index],
		)
	}
	return session
}

func buildEvent(
	observation sessionio.NativeObservation,
	event sessionio.Event,
) Event {
	text, removed := projectable(eventText(event))
	built := Event{
		Key:         string(event.ID),
		Kind:        string(event.Kind),
		Observation: string(observation.ID),
		OccurredAt:  event.Timestamp,
		Text:        text,
		RemovedNUL:  removed,
	}
	if event.Message != nil {
		built.Role = string(event.Message.Role)
	}
	if built.OccurredAt == nil {
		built.OccurredAt = observation.Timestamp
	}
	for _, evidence := range event.Evidence {
		built.Evidence = append(built.Evidence, Evidence{
			Observation: string(evidence.Observation),
			Locator:     evidence.Locator,
		})
	}
	return built
}

func passageKind(event Event) Kind {
	switch sessionio.EventKind(event.Kind) {
	case sessionio.EventKindReasoning:
		return KindReasoning
	case sessionio.EventKindToolCall:
		return KindToolCall
	case sessionio.EventKindToolResult:
		return KindToolResult
	case sessionio.EventKindMessage:
		switch sessionio.MessageRole(event.Role) {
		case sessionio.MessageRoleAssistant:
			return KindAssistant
		case sessionio.MessageRoleSystem, sessionio.MessageRoleDeveloper:
			return KindSystem
		default:
			return KindUser
		}
	default:
		return KindSystem
	}
}

func passageBody(session Session, passage Passage) string {
	parts := make([]string, 0, len(passage.Events))
	for _, index := range passage.Events {
		parts = append(parts, session.Events[index].Text)
	}
	return strings.Join(parts, "\n")
}

func passageLimitations(session Session, passage Passage) []Limitation {
	var removed int64
	for _, index := range passage.Events {
		removed += session.Events[index].RemovedNUL
	}
	var limitations []Limitation
	if removed > 0 {
		limitations = append(limitations, Limitation{
			Kind:         LimitationNULRemoved,
			RemovedBytes: removed,
		})
	}
	return limitations
}

func contentHash(kind Kind, body string) []byte {
	digest := sha256.New()
	digest.Write([]byte(ProjectionVersion))
	digest.Write([]byte{0})
	digest.Write([]byte(kind))
	digest.Write([]byte{0})
	digest.Write([]byte(body))
	return digest.Sum(nil)
}

func eventText(event sessionio.Event) string {
	switch {
	case event.Message != nil:
		return contentText(event.Message.Content)
	case event.Reasoning != nil:
		return joinNonEmpty(
			contentText(event.Reasoning.Content),
			contentText(event.Reasoning.Summary),
		)
	case event.ToolCall != nil:
		return joinNonEmpty(
			event.ToolCall.Name,
			payloadText(event.ToolCall.Input),
		)
	case event.ToolResult != nil:
		return payloadText(event.ToolResult.Output)
	default:
		return ""
	}
}

func contentText(blocks []sessionio.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text == nil {
			continue
		}
		if block.Availability != sessionio.ContentAvailabilityAvailable {
			continue
		}
		parts = append(parts, block.Text.Text)
	}
	return joinNonEmpty(parts...)
}

// payloadText keeps a tool payload only when it is valid text; binary payloads
// stay in the retained native record instead of the searchable projection.
func payloadText(payload sessionio.Payload) string {
	if len(payload.Data) == 0 || !utf8.Valid(payload.Data) {
		return ""
	}
	return string(payload.Data)
}

// projectable drops NUL, which real transcripts carry inside tool output and
// which no PostgreSQL text column can store, and reports how many bytes left.
// Every other control character survives. The native record keeps the byte and
// the evidence locator still addresses it.
func projectable(text string) (string, int64) {
	removed := int64(strings.Count(text, "\x00"))
	if removed == 0 {
		return text, 0
	}
	return strings.ReplaceAll(text, "\x00", ""), removed
}

func joinNonEmpty(parts ...string) string {
	selected := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		selected = append(selected, part)
	}
	return strings.Join(selected, "\n")
}

// Display renders a passage body as one bounded human line.
func Display(body string, limit int) string {
	collapsed := strings.Join(strings.Fields(body), " ")
	if limit <= 0 || len([]rune(collapsed)) <= limit {
		return collapsed
	}
	return string([]rune(collapsed)[:limit]) + "..."
}
