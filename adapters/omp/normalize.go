package omp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/sourceio"
)

type nativeMessage struct {
	Role       string          `json:"role"`
	Timestamp  json.RawMessage `json:"timestamp"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
	Model      string          `json:"model"`
	Provider   string          `json:"provider"`
	Usage      json.RawMessage `json:"usage"`
	Output     string          `json:"output"`
}

type nativeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Data      string          `json:"data"`
	MIMEType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolIndex struct {
	calls   []recordMeta
	results []recordMeta
}

func (state *readState) indexTools(message nativeMessage) error {
	if message.Role == "toolResult" {
		if message.ToolCallID == "" {
			return errors.New("OMP toolResult toolCallId is required")
		}
		index := state.tools[message.ToolCallID]
		if index == nil {
			index = &toolIndex{}
			state.tools[message.ToolCallID] = index
		}
		index.results = append(index.results, recordMeta{})
		return nil
	}
	if len(message.Content) == 0 {
		return nil
	}
	var blocks []json.RawMessage
	if message.Content[0] != '[' {
		return nil
	}
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return err
	}
	for _, raw := range blocks {
		var block nativeBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			return err
		}
		if block.Type != "toolCall" {
			continue
		}
		if block.ID == "" || len(block.Arguments) == 0 {
			return errors.New("OMP toolCall id and arguments are required")
		}
		index := state.tools[block.ID]
		if index == nil {
			index = &toolIndex{}
			state.tools[block.ID] = index
		}
		index.calls = append(index.calls, recordMeta{})
	}
	return nil
}

func (state *readState) next(ctx context.Context) (sessionio.ReadItem, error) {
	record, err := state.generation.Next(ctx)
	if errors.Is(err, io.EOF) {
		return state.nextExternal(ctx)
	}
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.readerError("read", state.session.ID, errorLocator(err, *state.session.Occurrence.Locator.File), err)
	}
	locator := record.SourceLocator(*state.session.Occurrence.Locator.File)
	value, err := parseEntry(record.Data)
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.readerError("read", state.session.ID, locator, err)
	}
	timestamp, dates := sourceTimestamp(value.Timestamp, false, "entry.timestamp", locator)
	observation := sessionio.NativeObservation{ID: state.observationID(record), NativeKind: value.Type, NativeKey: value.ID, NativeVersion: strconv.Itoa(state.version), Timestamp: timestamp, Locator: locator, Revision: state.generation.Revision(), Representation: record.NativeRepresentation()}
	item := sessionio.ReadItem{Session: state.session, Observation: observation, Diagnostics: dates}
	if err := state.normalize(&item, value); err != nil {
		return sessionio.ReadItem{}, state.adapter.readerError("read", state.session.ID, locator, err)
	}
	item.Observation.Limitations, err = state.limitations(record.Data)
	if err != nil {
		return sessionio.ReadItem{}, state.adapter.readerError("read", state.session.ID, locator, err)
	}
	current := recordMeta{id: observation.ID, locator: locator}
	if state.previous != nil {
		item.Relations = append(item.Relations, relation(sessionio.RelationKindPrevious, observationNode(current.id), observationNode(state.previous.id), sessionio.RelationOriginDeterministic, current), relation(sessionio.RelationKindNext, observationNode(state.previous.id), observationNode(current.id), sessionio.RelationOriginDeterministic, current))
	}
	if record.Record > state.headOrdinal {
		if state.version < 2 {
			if state.previous != nil && record.Record > state.headOrdinal+1 {
				item.Relations = append(item.Relations, relation(sessionio.RelationKindReplyTo, observationNode(current.id), observationNode(state.previous.id), sessionio.RelationOriginDeterministic, current))
			}
		} else if len(value.ParentID) != 0 && string(value.ParentID) != "null" {
			var parent string
			if err := json.Unmarshal(value.ParentID, &parent); err != nil {
				item.Diagnostics = append(item.Diagnostics, sessionio.Diagnostic{Code: "omp_parent_unresolved", Severity: sessionio.DiagnosticSeverityWarning, Message: "malformed native parentId: " + err.Error(), Locator: &locator, Cause: err})
			} else if targets := state.parents[parent]; len(targets) == 1 && targets[0].id != current.id && len(state.parents[value.ID]) == 1 {
				item.Relations = append(item.Relations, relation(sessionio.RelationKindReplyTo, observationNode(current.id), observationNode(targets[0].id), sessionio.RelationOriginNative, current))
			} else {
				item.Diagnostics = append(item.Diagnostics, diagnostic("omp_parent_unresolved", "parentId or entry id does not resolve uniquely", locator, nil))
			}
		}
		if record.Record == state.lastOrdinal {
			item.Relations = append(item.Relations, relation(sessionio.RelationKindActiveLeaf, sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(state.session.ID)}, observationNode(current.id), sessionio.RelationOriginDeterministic, current))
		}
	}
	if record.Record == state.headOrdinal && state.branchTarget != "" {
		item.Relations = append(item.Relations, relation(sessionio.RelationKindBranchParent, sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(state.session.ID)}, sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(state.branchTarget)}, sessionio.RelationOriginNative, current))
	}
	state.previous = &current
	for _, event := range item.Events {
		for _, block := range eventContents(event) {
			item.Relations = append(item.Relations, relation(sessionio.RelationKindContains, sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(event.ID)}, sessionio.NodeRef{Kind: sessionio.NodeKindContent, ID: string(block.ID)}, sessionio.RelationOriginDeterministic, current))
		}
		var callID string
		var call bool
		if event.ToolCall != nil {
			callID, call = event.ToolCall.CallID, true
		}
		if event.ToolResult != nil {
			callID = event.ToolResult.CallID
		}
		if callID == "" {
			continue
		}
		index := state.tools[callID]
		if index == nil || len(index.calls) != 1 || len(index.results) != 1 {
			item.Diagnostics = append(item.Diagnostics, diagnostic("omp_tool_pair_unresolved", "tool call/result candidates are missing or ambiguous", locator, nil))
			continue
		}
		meta := recordMeta{id: sessionio.ObservationID(event.ID), locator: locator}
		if call {
			index.calls[0] = meta
		} else {
			index.results[0] = meta
		}
		if index.calls[0].id != "" && index.results[0].id != "" {
			item.Relations = append(item.Relations, relation(sessionio.RelationKindToolPair, sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(index.calls[0].id)}, sessionio.NodeRef{Kind: sessionio.NodeKindEvent, ID: string(index.results[0].id)}, sessionio.RelationOriginNative, current))
		}
	}
	return item, nil
}

func (state *readState) normalize(item *sessionio.ReadItem, value entry) error {
	switch value.Type {
	case "session":
		facts := []sessionio.Fact{}
		if value.CWD != "" {
			facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindLaunchDirectory, Value: value.CWD})
		}
		var workspace struct {
			AdditionalDirectories []string `json:"additionalDirectories"`
		}
		if err := json.Unmarshal(item.Observation.Representation.Data, &workspace); err != nil {
			return err
		}
		for _, dir := range workspace.AdditionalDirectories {
			if dir != "" {
				facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindWorkingDirectory, Value: dir})
			}
		}
		appendFacts(item, facts)
		appendMarker(item, "session", "")
	case "message":
		var message nativeMessage
		if err := json.Unmarshal(value.Message, &message); err != nil {
			return err
		}
		timestamp, dates := sourceTimestamp(message.Timestamp, true, "message.timestamp", item.Observation.Locator)
		item.Diagnostics = append(item.Diagnostics, dates...)
		if err := state.messageEvents(item, message); err != nil {
			return err
		}
		for index := range item.Events {
			item.Events[index].Timestamp = timestamp
		}
	case "custom_message":
		appendMarker(item, value.Type, value.CustomType)
		return state.messageEvents(item, nativeMessage{Role: "system", Content: value.Content})
	case "compaction", "branch_summary":
		appendMarker(item, value.Type, "")
		if value.Summary != "" {
			return state.messageEvents(item, nativeMessage{Role: "system", Content: json.RawMessage(strconv.Quote(value.Summary))})
		}
	case "model_change":
		appendMarker(item, value.Type, "")
		if value.Model != "" {
			appendFacts(item, []sessionio.Fact{{Kind: sessionio.FactKindModel, Value: value.Model}})
		}
	case "thinking_level_change":
		appendMarker(item, value.Type, "")
		if value.ThinkingLevel != "" {
			appendFacts(item, []sessionio.Fact{{Kind: sessionio.FactKindEffort, Value: value.ThinkingLevel}})
		}
	case "custom":
		appendMarker(item, value.Type, value.CustomType)
	case "title", "title_change", "service_tier_change", "reset_boundary", "label", "ttsr_injection", "credential_pin", "session_init", "mode_change":
		appendMarker(item, value.Type, "")
	default:
		event := newEvent(item, sessionio.EventKindUnknown)
		event.Unknown = &sessionio.UnknownEvent{NativeType: value.Type}
		item.Events = append(item.Events, event)
		item.Diagnostics = append(item.Diagnostics, diagnostic("omp_unknown_record", "native record has no normalized projection", item.Observation.Locator, nil))
	}
	return nil
}

func (state *readState) messageEvents(item *sessionio.ReadItem, message nativeMessage) error {
	if message.Role == "toolResult" {
		if message.ToolCallID == "" {
			return errors.New("OMP toolResult toolCallId is required")
		}
		if len(message.Content) == 0 {
			return errors.New("OMP toolResult content is required")
		}
		status := sessionio.ToolResultStatusSuccess
		if message.IsError {
			status = sessionio.ToolResultStatusError
		}
		event := newEvent(item, sessionio.EventKindToolResult)
		event.ToolResult = &sessionio.ToolResultEvent{CallID: message.ToolCallID, Status: status, Output: sessionio.Payload{MediaType: "application/json", Data: message.Content}}
		item.Events = append(item.Events, event)
		return nil
	}
	role := sessionio.MessageRole(message.Role)
	switch role {
	case sessionio.MessageRoleUser, sessionio.MessageRoleAssistant, sessionio.MessageRoleDeveloper, sessionio.MessageRoleSystem:
	default:
		appendMarker(item, "message", message.Role)
		if message.Output != "" {
			message.Content = json.RawMessage(strconv.Quote(message.Output))
		}
		if len(message.Content) == 0 {
			return nil
		}
		role = sessionio.MessageRoleSystem
	}
	if len(message.Content) == 0 {
		if role == sessionio.MessageRoleUnknown {
			return nil
		}
		return errors.New("OMP message content is required")
	}
	content, reasoning, calls, err := state.contentBlocks(item, message.Content)
	if err != nil {
		return err
	}
	if len(content) != 0 || role == sessionio.MessageRoleAssistant {
		event := newEvent(item, sessionio.EventKindMessage)
		event.Message = &sessionio.MessageEvent{Role: role, Content: content}
		item.Events = append(item.Events, event)
	}
	if len(reasoning) != 0 {
		event := newEvent(item, sessionio.EventKindReasoning)
		event.Reasoning = &sessionio.ReasoningEvent{Content: reasoning}
		item.Events = append(item.Events, event)
	}
	for _, call := range calls {
		event := newEvent(item, sessionio.EventKindToolCall)
		event.ToolCall = &call
		item.Events = append(item.Events, event)
	}
	facts := []sessionio.Fact{}
	if message.Model != "" {
		facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindModel, Value: message.Model})
	}
	if message.Provider != "" {
		facts = append(facts, sessionio.Fact{Kind: sessionio.FactKindProvider, Value: message.Provider})
	}
	appendFacts(item, facts)
	if len(message.Usage) != 0 && string(message.Usage) != "null" {
		var usage struct {
			Input      *int64 `json:"input"`
			Output     *int64 `json:"output"`
			CacheRead  *int64 `json:"cacheRead"`
			CacheWrite *int64 `json:"cacheWrite"`
			Total      *int64 `json:"totalTokens"`
		}
		if err := json.Unmarshal(message.Usage, &usage); err != nil {
			return err
		}
		counters := []*int64{usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite, usage.Total}
		present := false
		for _, counter := range counters {
			if counter != nil {
				if *counter < 0 {
					return errors.New("negative OMP usage counter")
				}
				present = true
			}
		}
		if present {
			event := newEvent(item, sessionio.EventKindUsage)
			event.Usage = &sessionio.UsageEvent{InputTokens: usage.Input, OutputTokens: usage.Output, CacheReadTokens: usage.CacheRead, CacheWriteTokens: usage.CacheWrite, TotalTokens: usage.Total}
			item.Events = append(item.Events, event)
		}
	}
	return nil
}

func (state *readState) contentBlocks(item *sessionio.ReadItem, raw json.RawMessage) ([]sessionio.ContentBlock, []sessionio.ContentBlock, []sessionio.ToolCallEvent, error) {
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, nil, nil, err
		}
		return []sessionio.ContentBlock{state.textBlock(item, "text-0", text)}, nil, nil, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, nil, err
	}
	var content, reasoning []sessionio.ContentBlock
	var calls []sessionio.ToolCallEvent
	for index, data := range blocks {
		var block nativeBlock
		if err := json.Unmarshal(data, &block); err != nil {
			return nil, nil, nil, err
		}
		key := strconv.Itoa(index)
		switch block.Type {
		case "text":
			content = append(content, state.textBlock(item, key, block.Text))
		case "thinking":
			reasoning = append(reasoning, state.textBlock(item, key, block.Thinking))
		case "toolCall":
			if block.ID == "" || len(block.Arguments) == 0 {
				return nil, nil, nil, errors.New("OMP toolCall id and arguments are required")
			}
			calls = append(calls, sessionio.ToolCallEvent{CallID: block.ID, Name: block.Name, Input: sessionio.Payload{MediaType: "application/json", Data: block.Arguments}})
		case "image":
			media := &sessionio.MediaContent{MediaType: block.MIMEType}
			availability := sessionio.ContentAvailabilityAvailable
			if strings.HasPrefix(block.Data, "blob:") {
				media.Reference = block.Data
				availability = sessionio.ContentAvailabilityExternal
				if external := state.external[block.Data]; external == nil || external.missing || external.oversized {
					availability = sessionio.ContentAvailabilityUnavailable
				}
			} else {
				decoded, err := base64.StdEncoding.DecodeString(block.Data)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("invalid inline OMP image: %w", err)
				}
				media.Data = decoded
			}
			content = append(content, sessionio.ContentBlock{ID: contentID(item, key), Kind: sessionio.ContentKindMedia, Availability: availability, Media: media})
		default:
			nativeType := block.Type
			if nativeType == "" {
				nativeType = "opaque"
			}
			content = append(content, sessionio.ContentBlock{ID: contentID(item, key), Kind: sessionio.ContentKindOpaque, Availability: sessionio.ContentAvailabilityAvailable, Opaque: &sessionio.OpaqueContent{NativeType: nativeType, MediaType: "application/json", Data: data}})
		}
	}
	return content, reasoning, calls, nil
}
func (state *readState) textBlock(item *sessionio.ReadItem, key, text string) sessionio.ContentBlock {
	return sessionio.ContentBlock{ID: contentID(item, key), Kind: sessionio.ContentKindText, Availability: sessionio.ContentAvailabilityAvailable, Text: &sessionio.TextContent{Text: text}}
}
func contentID(item *sessionio.ReadItem, key string) sessionio.ContentID {
	return sessionio.ContentID(sourceio.DerivedID("content", string(item.Observation.ID), key))
}
func newEvent(item *sessionio.ReadItem, kind sessionio.EventKind) sessionio.Event {
	return sessionio.Event{ID: sessionio.EventID(sourceio.DerivedID("event", string(item.Observation.ID), strconv.Itoa(len(item.Events)), string(kind))), Kind: kind, Timestamp: item.Observation.Timestamp, Evidence: []sessionio.EvidenceRef{{Observation: item.Observation.ID, Locator: item.Observation.Locator}}}
}
func appendMarker(item *sessionio.ReadItem, name, state string) {
	event := newEvent(item, sessionio.EventKindMarker)
	event.Marker = &sessionio.MarkerEvent{Name: name, State: state}
	item.Events = append(item.Events, event)
}
func appendFacts(item *sessionio.ReadItem, facts []sessionio.Fact) {
	if len(facts) == 0 {
		return
	}
	event := newEvent(item, sessionio.EventKindFacts)
	event.Facts = &sessionio.FactEvent{Facts: facts}
	item.Events = append(item.Events, event)
}
func eventContents(event sessionio.Event) []sessionio.ContentBlock {
	if event.Message != nil {
		return event.Message.Content
	}
	if event.Reasoning != nil {
		return event.Reasoning.Content
	}
	return []sessionio.ContentBlock{}
}
func observationNode(id sessionio.ObservationID) sessionio.NodeRef {
	return sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(id)}
}
func relation(kind sessionio.RelationKind, from, to sessionio.NodeRef, origin sessionio.RelationOrigin, evidence recordMeta) sessionio.Relation {
	return sessionio.Relation{ID: sessionio.RelationID(sourceio.DerivedID("relation", string(kind), string(from.Kind), from.ID, string(to.Kind), to.ID)), Kind: kind, From: from, To: to, Origin: origin, Evidence: []sessionio.EvidenceRef{{Observation: evidence.id, Locator: evidence.locator}}}
}
