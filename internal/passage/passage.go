// Package passage turns normalized reader items into deterministic passages
// and their searchable projections.
package passage

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	sessionio "github.com/nikitatsym/agent-session-io"
)

// BuilderVersion identifies this passage builder in every stored passage row.
const BuilderVersion = "sessionio.passage/v2"

// ProjectionVersion identifies the projection builder in every stored
// projection row. It changes independently of BuilderVersion.
const ProjectionVersion = "sessionio.projection/v2"

// MaxBodyBytes bounds one projection body. A larger structural unit is split
// on native boundaries first and only then on a rune-safe window.
const MaxBodyBytes = 64 * 1024

// Kind is the structural class of one passage.
type Kind string

const (
	KindUser             Kind = "user"
	KindAssistant        Kind = "assistant"
	KindSystem           Kind = "system"
	KindReasoning        Kind = "reasoning"
	KindReasoningSummary Kind = "reasoning_summary"
	KindToolCall         Kind = "tool_call"
	KindToolResult       Kind = "tool_result"
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
	// removals locate the U+0000 bytes dropped from Text, so a split body can
	// report each loss on the part whose own bytes carried it.
	removals []nulRun
}

// nulRun is one run of removed U+0000 bytes at an offset in projected text.
type nulRun struct {
	offset int64
	count  int64
}

// Passage groups contiguous events of one structural class. A structural unit
// larger than MaxBodyBytes becomes Parts passages that share their events.
type Passage struct {
	Kind Kind
	// Events indexes into Session.Events in source order.
	Events      []int
	Body        string
	ContentHash []byte
	OccurredAt  *time.Time
	Limitations []Limitation
	Part        int
	Parts       int
}

// Relation is one retained structural relation between typed nodes.
type Relation struct {
	Kind        string
	Origin      string
	FromKind    string
	FromRef     string
	ToKind      string
	ToRef       string
	Observation string
}

// Session carries every retained event, passage, and relation of one reader
// session.
type Session struct {
	Events    []Event
	Passages  []Passage
	Relations []Relation
}

// segment is one projectable text of one event. Reasoning content and a
// separately exposed reasoning summary are distinct segments.
type segment struct {
	event    int
	kind     Kind
	text     string
	removals []nulRun
}

// Build is deterministic: the same reader items always produce the same
// passages, bodies, and content hashes.
func Build(items []sessionio.ReadItem) Session {
	var session Session
	var segments []segment
	for itemIndex := range items {
		for eventIndex := range items[itemIndex].Events {
			native := items[itemIndex].Events[eventIndex]
			event := buildEvent(items[itemIndex].Observation, native)
			index := len(session.Events)
			session.Events = append(session.Events, event)
			segments = append(segments, eventSegments(index, event, native)...)
		}
		for _, relation := range items[itemIndex].Relations {
			session.Relations = append(session.Relations, buildRelation(relation))
		}
	}
	session.Passages = groupPassages(session.Events, segments)
	return session
}

// groupPassages merges contiguous assistant segments and splits any body that
// exceeds MaxBodyBytes.
func groupPassages(events []Event, segments []segment) []Passage {
	type group struct {
		kind     Kind
		events   []int
		texts    []string
		removals []nulRun
		// length is the joined body length, which turns a segment-relative
		// removal offset into a body-relative one.
		length   int64
		occurred *time.Time
	}
	var groups []group
	open := -1
	for _, current := range segments {
		if current.text == "" {
			continue
		}
		if current.kind == KindAssistant && open >= 0 {
			base := groups[open].length + 1
			groups[open].events = append(groups[open].events, current.event)
			groups[open].texts = append(groups[open].texts, current.text)
			groups[open].removals = append(
				groups[open].removals,
				shiftRuns(current.removals, base)...,
			)
			groups[open].length = base + int64(len(current.text))
			continue
		}
		groups = append(groups, group{
			kind:     current.kind,
			events:   []int{current.event},
			texts:    []string{current.text},
			removals: current.removals,
			length:   int64(len(current.text)),
			occurred: events[current.event].OccurredAt,
		})
		open = -1
		if current.kind == KindAssistant {
			open = len(groups) - 1
		}
	}
	var passages []Passage
	for _, built := range groups {
		bodies := splitBody(strings.Join(built.texts, "\n"))
		limitations := splitLimitations(built.removals, bodies)
		for part, body := range bodies {
			passages = append(passages, Passage{
				Kind:        built.kind,
				Events:      built.events,
				Body:        body,
				ContentHash: contentHash(built.kind, part, body),
				OccurredAt:  built.occurred,
				Limitations: limitations[part],
				Part:        part,
				Parts:       len(bodies),
			})
		}
	}
	return passages
}

func shiftRuns(runs []nulRun, base int64) []nulRun {
	shifted := make([]nulRun, len(runs))
	for index, run := range runs {
		shifted[index] = nulRun{offset: run.offset + base, count: run.count}
	}
	return shifted
}

// splitBody prefers native line boundaries and falls back to a rune-safe
// window only for a structurally indivisible span.
func splitBody(body string) []string {
	if len(body) <= MaxBodyBytes {
		return []string{body}
	}
	var parts []string
	var current strings.Builder
	for _, line := range splitLines(body) {
		if current.Len() > 0 && current.Len()+len(line) > MaxBodyBytes {
			parts = append(parts, current.String())
			current.Reset()
		}
		if len(line) <= MaxBodyBytes {
			current.WriteString(line)
			continue
		}
		for _, window := range splitRunes(line) {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
			if len(window) == MaxBodyBytes {
				parts = append(parts, window)
				continue
			}
			current.WriteString(window)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// splitLines keeps every terminator with its line, so the concatenated parts
// reproduce the body byte for byte.
func splitLines(body string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(body); index++ {
		if body[index] == '\n' {
			lines = append(lines, body[start:index+1])
			start = index + 1
		}
	}
	if start < len(body) {
		lines = append(lines, body[start:])
	}
	return lines
}

// splitRunes cuts a single oversized line without ever splitting a rune.
func splitRunes(line string) []string {
	var windows []string
	start := 0
	for start < len(line) {
		end := start + MaxBodyBytes
		if end >= len(line) {
			windows = append(windows, line[start:])
			break
		}
		for end > start && !utf8.RuneStart(line[end]) {
			end--
		}
		windows = append(windows, line[start:end])
		start = end
	}
	return windows
}

func buildEvent(
	observation sessionio.NativeObservation,
	event sessionio.Event,
) Event {
	text, removals := projectable(eventText(event))
	built := Event{
		Key:         string(event.ID),
		Kind:        string(event.Kind),
		Observation: string(observation.ID),
		OccurredAt:  event.Timestamp,
		Text:        text,
		removals:    removals,
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

func buildRelation(relation sessionio.Relation) Relation {
	built := Relation{
		Kind:     string(relation.Kind),
		Origin:   string(relation.Origin),
		FromKind: string(relation.From.Kind),
		FromRef:  relation.From.ID,
		ToKind:   string(relation.To.Kind),
		ToRef:    relation.To.ID,
	}
	if len(relation.Evidence) > 0 {
		built.Observation = string(relation.Evidence[0].Observation)
	}
	return built
}

func eventSegments(
	index int,
	event Event,
	native sessionio.Event,
) []segment {
	if native.Reasoning != nil {
		content, contentRemovals := projectable(
			contentText(native.Reasoning.Content),
		)
		summary, summaryRemovals := projectable(
			contentText(native.Reasoning.Summary),
		)
		return []segment{
			{
				event:    index,
				kind:     KindReasoning,
				text:     content,
				removals: contentRemovals,
			},
			{
				event:    index,
				kind:     KindReasoningSummary,
				text:     summary,
				removals: summaryRemovals,
			},
		}
	}
	return []segment{{
		event:    index,
		kind:     passageKind(event),
		text:     event.Text,
		removals: event.removals,
	}}
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

// splitLimitations gives every part exactly the bytes its own body lost. The
// ordered runs are walked once; a run on a part boundary belongs to the part
// that follows it, and the last part also owns a run at the end of the body.
func splitLimitations(removals []nulRun, bodies []string) [][]Limitation {
	limitations := make([][]Limitation, len(bodies))
	cursor := 0
	var start int64
	for part, body := range bodies {
		end := start + int64(len(body))
		last := part == len(bodies)-1
		var removed int64
		for cursor < len(removals) {
			offset := removals[cursor].offset
			if offset > end || (offset == end && !last) {
				break
			}
			removed += removals[cursor].count
			cursor++
		}
		if removed > 0 {
			limitations[part] = []Limitation{{
				Kind:         LimitationNULRemoved,
				RemovedBytes: removed,
			}}
		}
		start = end
	}
	return limitations
}

func contentHash(kind Kind, part int, body string) []byte {
	digest := sha256.New()
	digest.Write([]byte(ProjectionVersion))
	digest.Write([]byte{0})
	digest.Write([]byte(kind))
	digest.Write([]byte{0})
	digest.Write([]byte(strconv.Itoa(part)))
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
// which no PostgreSQL text column can store, and reports where the bytes left.
// Every other control character survives. The native record keeps the byte and
// the evidence locator still addresses it.
func projectable(text string) (string, []nulRun) {
	index := strings.IndexByte(text, 0)
	if index < 0 {
		return text, nil
	}
	var cleaned strings.Builder
	cleaned.Grow(len(text))
	var runs []nulRun
	rest := text
	for index >= 0 {
		cleaned.WriteString(rest[:index])
		count := int64(0)
		for index < len(rest) && rest[index] == 0 {
			index++
			count++
		}
		runs = append(runs, nulRun{offset: int64(cleaned.Len()), count: count})
		rest = rest[index:]
		index = strings.IndexByte(rest, 0)
	}
	cleaned.WriteString(rest)
	return cleaned.String(), runs
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
