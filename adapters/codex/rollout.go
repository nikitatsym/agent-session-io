package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nikitatsym/agent-session-io"
)

type completedItemRecord struct {
	Item completedItem `json:"item"`
}

type completedItem struct {
	Type       string          `json:"type"`
	Kind       string          `json:"kind"`
	ID         string          `json:"id"`
	Content    json.RawMessage `json:"content"`
	RawContent []string        `json:"raw_content"`
	Summary    []string        `json:"summary_text"`
	Status     string          `json:"status"`
	ExitCode   *int            `json:"exit_code"`
	Result     *struct {
		IsError bool `json:"isError"`
	} `json:"result"`
}

func completionToolResult(subtype, extension string) bool {
	switch subtype {
	case "CommandExecution", "FileChange", "McpToolCall":
		return true
	case "Extension":
		return extension == "web.search"
	default:
		return false
	}
}

func normalizeCompletedItem(observation sessionio.NativeObservation, raw json.RawMessage, event func(sessionio.EventKind) sessionio.Event) (sessionio.Event, *sessionio.Diagnostic, error) {
	var envelope struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return sessionio.Event{}, nil, err
	}
	if err := requireObject(envelope.Item, "item_completed item"); err != nil {
		return sessionio.Event{}, nil, err
	}
	var subtype struct {
		Type string `json:"type"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(envelope.Item, &subtype); err != nil {
		return sessionio.Event{}, nil, err
	}
	if subtype.Type == "" {
		return sessionio.Event{}, nil, errors.New("item_completed item type is required")
	}
	known := subtype.Type == "UserMessage" || subtype.Type == "AgentMessage" || subtype.Type == "Reasoning" || completionToolResult(subtype.Type, subtype.Kind)
	native := completedItem{Type: subtype.Type, Kind: subtype.Kind}
	if known {
		if err := json.Unmarshal(envelope.Item, &native); err != nil {
			return sessionio.Event{}, nil, fmt.Errorf("item_completed item: %w", err)
		}
	}
	switch native.Type {
	case "UserMessage", "AgentMessage":
		item := event(sessionio.EventKindMessage)
		content, err := completedMessageContent(item.ID, native.Content, native.Type)
		if err != nil {
			return sessionio.Event{}, nil, err
		}
		messageRole := sessionio.MessageRoleUser
		if native.Type == "AgentMessage" {
			messageRole = sessionio.MessageRoleAssistant
		}
		item.Message = &sessionio.MessageEvent{Role: messageRole, Content: content}
		return item, nil, nil
	case "Reasoning":
		var fields struct {
			RawContent json.RawMessage `json:"raw_content"`
			Summary    json.RawMessage `json:"summary_text"`
		}
		if err := json.Unmarshal(envelope.Item, &fields); err != nil {
			return sessionio.Event{}, nil, err
		}
		if len(fields.RawContent) == 0 || len(fields.Summary) == 0 || string(fields.RawContent) == "null" || string(fields.Summary) == "null" {
			return sessionio.Event{}, nil, errors.New("item_completed Reasoning raw_content and summary_text arrays are required")
		}
		if len(native.RawContent) == 0 && len(native.Summary) == 0 {
			item := event(sessionio.EventKindMarker)
			item.Marker = &sessionio.MarkerEvent{Name: "item_completed", State: native.Type}
			return item, nil, nil
		}
		item := event(sessionio.EventKindReasoning)
		reasoning := &sessionio.ReasoningEvent{}
		for _, text := range native.RawContent {
			if text != "" {
				reasoning.Content = append(reasoning.Content, textBlock(item.ID, len(reasoning.Content), text))
			}
		}
		for _, text := range native.Summary {
			if text != "" {
				reasoning.Summary = append(reasoning.Summary, textBlock(item.ID, len(reasoning.Content)+len(reasoning.Summary), text))
			}
		}
		if len(reasoning.Content) == 0 && len(reasoning.Summary) == 0 {
			item = event(sessionio.EventKindMarker)
			item.Marker = &sessionio.MarkerEvent{Name: "item_completed", State: native.Type}
			return item, nil, nil
		}
		item.Reasoning = reasoning
		return item, nil, nil
	case "CommandExecution", "FileChange", "McpToolCall", "Extension":
		if native.Type == "Extension" && native.Kind == "" {
			return sessionio.Event{}, nil, errors.New("item_completed Extension kind is required")
		}
		if completionToolResult(native.Type, native.Kind) {
			if native.ID == "" {
				return sessionio.Event{}, nil, errors.New("item_completed tool item id is required")
			}
			if native.Type != "Extension" && native.Status == "" {
				return sessionio.Event{}, nil, errors.New("item_completed tool status is required")
			}
			statusRecord := nativeRecord{Status: native.Status}
			if native.Type == "CommandExecution" {
				statusRecord.ExitCode = native.ExitCode
			}
			status := operationalStatus(statusRecord)
			if native.Status == "failed" || native.Status == "error" || (native.Type == "McpToolCall" && native.Result != nil && native.Result.IsError) {
				status = sessionio.ToolResultStatusError
			}
			if native.Type == "Extension" {
				status = sessionio.ToolResultStatusUnknown
			}
			item := event(sessionio.EventKindToolResult)
			item.ToolResult = &sessionio.ToolResultEvent{CallID: native.ID, Status: status, Output: sessionio.Payload{MediaType: "application/json", Data: cloneRaw(envelope.Item)}}
			return item, nil, nil
		}
	}
	unknownSubtype := "item_completed." + native.Type
	if native.Type == "Extension" {
		unknownSubtype += "." + native.Kind
	}
	item := event(sessionio.EventKindUnknown)
	item.Unknown = &sessionio.UnknownEvent{NativeType: unknownSubtype}
	locator := observation.Locator
	return item, &sessionio.Diagnostic{Code: "codex_unknown_item_subtype", Severity: sessionio.DiagnosticSeverityWarning, Message: fmt.Sprintf("Codex subtype %q has no normalized projection", unknownSubtype), Locator: &locator}, nil
}

func completedMessageContent(eventID sessionio.EventID, raw json.RawMessage, subtype string) ([]sessionio.ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("item_completed message content is required")
	}
	var nativeBlocks []json.RawMessage
	if err := json.Unmarshal(raw, &nativeBlocks); err != nil {
		return nil, fmt.Errorf("item_completed message content: %w", err)
	}
	blocks := make([]sessionio.ContentBlock, 0, len(nativeBlocks))
	for index, rawBlock := range nativeBlocks {
		var native struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal(rawBlock, &native); err != nil {
			return nil, err
		}
		if native.Type == "" {
			return nil, errors.New("item_completed content block type is required")
		}
		if (subtype == "UserMessage" && native.Type == "text") || (subtype == "AgentMessage" && native.Type == "Text") {
			if len(native.Text) == 0 || string(native.Text) == "null" {
				return nil, errors.New("item_completed text content requires text")
			}
			var text string
			if err := json.Unmarshal(native.Text, &text); err != nil {
				return nil, err
			}
			blocks = append(blocks, textBlock(eventID, index, text))
		} else {
			blocks = append(blocks, opaqueBlock(eventID, index, native.Type, sessionio.ContentAvailabilityAvailable, "application/json", rawBlock))
		}
	}
	return blocks, nil
}

func threadUsage(raw json.RawMessage) (sessionio.UsageEvent, error) {
	if err := requireObject(raw, "token_usage_record payload"); err != nil {
		return sessionio.UsageEvent{}, err
	}
	var record struct {
		Thread *struct {
			Input      *int64 `json:"input_tokens"`
			Output     *int64 `json:"output_tokens"`
			Total      *int64 `json:"total_tokens"`
			CacheRead  *int64 `json:"cached_input_tokens"`
			CacheWrite *int64 `json:"cache_write_input_tokens"`
			Reasoning  *int64 `json:"reasoning_output_tokens"`
		} `json:"thread_token_usage"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return sessionio.UsageEvent{}, err
	}
	if record.Thread == nil {
		return sessionio.UsageEvent{}, errors.New("token_usage_record thread_token_usage is required")
	}
	usage := sessionio.UsageEvent{InputTokens: record.Thread.Input, OutputTokens: record.Thread.Output, TotalTokens: record.Thread.Total, CacheReadTokens: record.Thread.CacheRead, CacheWriteTokens: record.Thread.CacheWrite, ReasoningTokens: record.Thread.Reasoning}
	if !hasUsageCounters(usage) {
		return sessionio.UsageEvent{}, errors.New("token_usage_record thread_token_usage has no supported counters")
	}
	return usage, nil
}

func hasUsageCounters(usage sessionio.UsageEvent) bool {
	return usage.InputTokens != nil || usage.OutputTokens != nil || usage.TotalTokens != nil || usage.CacheReadTokens != nil || usage.CacheWriteTokens != nil || usage.ReasoningTokens != nil
}

func rolloutFacts(kind string, raw json.RawMessage) ([]sessionio.Fact, string, error) {
	if err := requireObject(raw, kind+" payload"); err != nil {
		return nil, "", err
	}
	facts := make([]sessionio.Fact, 0, 5)
	appendFact := func(kind sessionio.FactKind, value string) {
		if value != "" {
			facts = append(facts, sessionio.Fact{Kind: kind, Value: value})
		}
	}
	if kind == "world_state" {
		var record struct {
			Full  *bool           `json:"full"`
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, "", err
		}
		if record.Full == nil {
			return nil, "", errors.New("world_state full is required")
		}
		if err := requireObject(record.State, "world_state state"); err != nil {
			return nil, "", err
		}
		var state struct {
			Model        string `json:"model"`
			Environments struct {
				CurrentDate  string `json:"current_date"`
				Timezone     string `json:"timezone"`
				Environments struct {
					Local struct {
						CWD string `json:"cwd"`
					} `json:"local"`
				} `json:"environments"`
			} `json:"environments"`
		}
		if err := json.Unmarshal(record.State, &state); err != nil {
			return nil, "", err
		}
		appendFact(sessionio.FactKindModel, state.Model)
		appendFact(sessionio.FactKindWorkingDirectory, state.Environments.Environments.Local.CWD)
		appendFact(sessionio.FactKindCurrentDate, state.Environments.CurrentDate)
		appendFact(sessionio.FactKindTimezone, state.Environments.Timezone)
		markerState := "delta"
		if *record.Full {
			markerState = "full"
		}
		return facts, markerState, nil
	}
	var envelope struct {
		Settings json.RawMessage `json:"thread_settings"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", err
	}
	if err := requireObject(envelope.Settings, "thread_settings_applied thread_settings"); err != nil {
		return nil, "", err
	}
	var settings struct {
		Model         string `json:"model"`
		Provider      string `json:"model_provider_id"`
		CWD           string `json:"cwd"`
		Approval      string `json:"approval_policy"`
		Collaboration struct {
			Settings struct {
				Effort *string `json:"reasoning_effort"`
			} `json:"settings"`
		} `json:"collaboration_mode"`
	}
	if err := json.Unmarshal(envelope.Settings, &settings); err != nil {
		return nil, "", err
	}
	appendFact(sessionio.FactKindModel, settings.Model)
	appendFact(sessionio.FactKindProvider, settings.Provider)
	appendFact(sessionio.FactKindWorkingDirectory, settings.CWD)
	appendFact(sessionio.FactKindApprovalPolicy, settings.Approval)
	if settings.Collaboration.Settings.Effort != nil {
		appendFact(sessionio.FactKindEffort, *settings.Collaboration.Settings.Effort)
	}
	return facts, "", nil
}

func requireObject(raw json.RawMessage, field string) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return fmt.Errorf("%s must be a JSON object", field)
	}
	return nil
}
