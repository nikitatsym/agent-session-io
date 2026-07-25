package contractstress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const fixtureRoot = "contractstress-root"

type ompRecord struct {
	Version           int             `json:"version"`
	Type              string          `json:"type"`
	ID                string          `json:"id"`
	Title             string          `json:"title"`
	CWD               string          `json:"cwd"`
	Timestamp         string          `json:"timestamp"`
	ParentSessionPath string          `json:"parentSession"`
	ParentID          json.RawMessage `json:"parentId"`
	Message           *ompMessage     `json:"message"`
	Summary           string          `json:"summary"`
	FirstKeptEntryID  string          `json:"firstKeptEntryId"`
	TokensBefore      int             `json:"tokensBefore"`
	CustomType        string          `json:"customType"`
	Data              json.RawMessage `json:"data"`
}

type ompMessage struct {
	Role       string            `json:"role"`
	ToolCallID string            `json:"toolCallId"`
	ToolName   string            `json:"toolName"`
	IsError    *bool             `json:"isError"`
	Content    []json.RawMessage `json:"content"`
}

type ompContentBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text"`
	Data      string          `json:"data"`
	MIMEType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type manifest struct {
	Database struct {
		Path        string `json:"path"`
		SHA256      string `json:"sha256"`
		WALPath     string `json:"wal_path"`
		WALSHA256   string `json:"wal_sha256"`
		EventPath   string `json:"event_path"`
		EventSHA256 string `json:"event_sha256"`
	} `json:"database"`
	TransactionRevision string                         `json:"transaction_revision"`
	EventRevision       string                         `json:"event_revision"`
	RequiredSchema      map[string][]string            `json:"required_schema"`
	SchemaVariants      map[string]map[string][]string `json:"schema_variants"`
	Rows                []manifestRow                  `json:"rows"`
	Events              []manifestEvent                `json:"events"`
}

type manifestRow struct {
	NativeKind          string              `json:"native_kind"`
	Table               string              `json:"table"`
	Keys                [][]string          `json:"keys"`
	TransactionRevision string              `json:"transaction_revision,omitempty"`
	Cells               [][]json.RawMessage `json:"cells"`
}

type manifestEvent struct {
	Sequence int        `json:"sequence"`
	Table    string     `json:"table"`
	Keys     [][]string `json:"keys"`
}

func TestOMPContract(t *testing.T) {
	records, revision := readOMP(t, "omp-append.jsonl")
	session, items := projectOMP(t, records, revision)
	if session.NativeID != "omp-session-01" || session.Title != "Synthetic OMP branches" {
		t.Fatalf("OMP session = %#v", session)
	}
	if len(session.Native.Relationships) != 0 {
		t.Fatalf("OMP parent session path must not become an unresolved native relation: %#v", session.Native)
	}
	if !bytes.Contains(records[0].Data, []byte(`"parentSession":"/synthetic/omp-parent.jsonl"`)) {
		t.Fatalf("OMP parent session path was not preserved in native header: %s", records[0].Data)
	}
	var rebuilt []byte
	for _, record := range records {
		rebuilt = append(rebuilt, record.Data...)
		rebuilt = append(rebuilt, record.Framing...)
	}
	want, err := os.ReadFile(fixturePath("omp-append.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, want) {
		t.Fatal("OMP records did not reconstruct physical JSONL")
	}

	var active, reply int
	var verifiedReference, externalLimitation, missingReference, missingLimitation bool
	for _, item := range items {
		for _, limit := range item.Observation.Limitations {
			externalLimitation = externalLimitation || (limit.Kind == sessionio.LimitationKindExternalPayload &&
				limit.Detail == "verified external synthetic blob is not imported")
			missingLimitation = missingLimitation || limit.Kind == sessionio.LimitationKindMissingExternalPayload
		}
		for _, event := range item.Events {
			if event.Message == nil {
				continue
			}
			for _, block := range event.Message.Content {
				if block.Media == nil {
					continue
				}
				verifiedReference = verifiedReference || (block.Media.Reference == "blob:sha256:345e3097da4a84f723384a45ae77cf40e402b8d30bdc8f4cc44bde079eac5fb9" && block.Availability == sessionio.ContentAvailabilityExternal)
				missingReference = missingReference || (block.Media.Reference == "blob:sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" && block.Availability == sessionio.ContentAvailabilityUnavailable)
			}
		}
		for _, rel := range item.Relations {
			switch rel.Kind {
			case sessionio.RelationKindReplyTo:
				reply++
			case sessionio.RelationKindActiveLeaf:
				active++
				if rel.From.ID != string(session.ID) || rel.To.ID != "omp-observation-entry-custom" ||
					rel.Origin != sessionio.RelationOriginDeterministic || len(rel.Evidence) != 1 {
					t.Fatalf("active leaf = %#v", rel)
				}
			}
		}
	}
	if active != 1 || reply != 5 || !verifiedReference || !externalLimitation || !missingReference || !missingLimitation {
		t.Fatalf("OMP topology=%d/%d verified_ref=%t external_limit=%t missing_ref=%t missing_limit=%t", active, reply, verifiedReference, externalLimitation, missingReference, missingLimitation)
	}
	assertRecordsValidate(t, session, items)
}

func TestOMPLifecycle(t *testing.T) {
	before, err := os.ReadFile(fixturePath("omp-before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	appendData, err := os.ReadFile(fixturePath("omp-append.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rewriteData, err := os.ReadFile(fixturePath("omp-rewrite.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	token := consumeOMP(t, path)
	if err := os.WriteFile(path, appendData, 0o600); err != nil {
		t.Fatal(err)
	}
	appendResult := openOMP(t, path, &token)
	if appendResult.Reconciliation != (sourceio.Reconciliation{Change: sourceio.FileChangeGrown, Resume: sourceio.ResumeContinue}) {
		t.Fatalf("append reconciliation = %#v", appendResult.Reconciliation)
	}
	if got := len(consume(t, appendResult.Generation)); got != 4 {
		t.Fatalf("append records = %d, want 4", got)
	}

	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	token = consumeOMP(t, path)
	replacement := filepath.Join(filepath.Dir(path), "replacement.jsonl")
	if err := os.WriteFile(replacement, rewriteData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	rewriteResult := openOMP(t, path, &token)
	if rewriteResult.Reconciliation != (sourceio.Reconciliation{Change: sourceio.FileChangeReplaced, Resume: sourceio.ResumeReplay}) {
		t.Fatalf("rewrite reconciliation = %#v", rewriteResult.Reconciliation)
	}
	if got := len(consume(t, rewriteResult.Generation)); got != 3 {
		t.Fatalf("rewrite records = %d, want 3", got)
	}
}

func TestOMPParentDiagnostics(t *testing.T) {
	input := []byte("{\"type\":\"session\",\"version\":3,\"id\":\"omp-errors\",\"title\":\"Synthetic errors\",\"timestamp\":\"2026-07-25T00:00:00Z\",\"cwd\":\"/synthetic\"}\n" +
		"{\"type\":\"message\",\"id\":\"duplicate\",\"parentId\":null,\"timestamp\":\"2026-07-25T00:00:01Z\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"one\"}]}}\n" +
		"{\"type\":\"message\",\"id\":\"duplicate\",\"parentId\":\"duplicate\",\"timestamp\":\"2026-07-25T00:00:02Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"two\"}]}}\n" +
		"{\"type\":\"message\",\"id\":\"ambiguous\",\"parentId\":\"duplicate\",\"timestamp\":\"2026-07-25T00:00:03Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"three\"}]}}\n" +
		"{\"type\":\"message\",\"id\":\"missing\",\"parentId\":\"gone\",\"timestamp\":\"2026-07-25T00:00:04Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"four\"}]}}\n" +
		"{\"type\":\"message\",\"id\":\"malformed\",\"parentId\":17,\"timestamp\":\"2026-07-25T00:00:05Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"five\"}]}}\n")
	path := filepath.Join(t.TempDir(), "parents.jsonl")
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}
	result := openOMP(t, path, nil)
	records := consume(t, result.Generation)
	_, items := projectOMP(t, records, result.Generation.Revision())
	var ambiguous, missing, malformed, malformedObservation bool
	for _, item := range items {
		malformedObservation = malformedObservation || (item.Observation.ID == "omp-observation-malformed" &&
			bytes.Contains(item.Observation.Representation.Data, []byte(`"parentId":17`)))
		for _, diagnostic := range item.Diagnostics {
			ambiguous = ambiguous || diagnostic.Code == "omp_ambiguous_parent"
			missing = missing || diagnostic.Code == "omp_missing_parent"
			malformed = malformed || diagnostic.Code == "omp_malformed_parent"
		}
		for _, relation := range item.Relations {
			if relation.Kind == sessionio.RelationKindReplyTo &&
				(item.Observation.ID == "omp-observation-ambiguous" || item.Observation.ID == "omp-observation-missing" || item.Observation.ID == "omp-observation-malformed") {
				t.Fatalf("invented parent relation for %s", item.Observation.ID)
			}
		}
	}
	if !ambiguous || !missing || !malformed || !malformedObservation {
		t.Fatalf("diagnostics ambiguous=%t missing=%t malformed=%t observation=%t", ambiguous, missing, malformed, malformedObservation)
	}
}

func readOMP(t *testing.T, name string) ([]sourceio.JSONLRecord, sessionio.Revision) {
	t.Helper()
	result := openOMP(t, fixturePath(name), nil)
	records := consume(t, result.Generation)
	return records, result.Generation.Revision()
}

func openOMP(t *testing.T, path string, resume *sourceio.ResumeToken) sourceio.OpenResult {
	t.Helper()
	result, err := sourceio.OpenJSONLGeneration(context.Background(), sourceio.FileSpec{
		OpenPath: path,
		Locator:  sessionio.FileLocator{Root: fixtureRoot, Path: "omp/session.jsonl"},
	}, sourceio.OpenOptions{
		TailMode:   sourceio.TailModeFinal,
		SizePolicy: sourceio.RecordSizePolicy{MaxBytes: sourceio.UnlimitedRecordBytes},
		Resume:     resume,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func consumeOMP(t *testing.T, path string) sourceio.ResumeToken {
	t.Helper()
	result := openOMP(t, path, nil)
	consume(t, result.Generation)
	return result.Generation.ResumeToken()
}

func consume(t *testing.T, generation *sourceio.JSONLGeneration) []sourceio.JSONLRecord {
	t.Helper()
	var result []sourceio.JSONLRecord
	for {
		record, err := generation.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, record)
	}
	if err := generation.Close(); err != nil {
		t.Fatal(err)
	}
	return result
}

func projectOMP(t *testing.T, records []sourceio.JSONLRecord, revision sessionio.Revision) (sessionio.SessionRef, []sessionio.ReadItem) {
	t.Helper()
	if len(records) == 0 {
		t.Fatal("OMP fixture has no records")
	}
	parsed := make([]ompRecord, len(records))
	for index, record := range records {
		if err := json.Unmarshal(record.Data, &parsed[index]); err != nil {
			t.Fatalf("OMP record %d: %v", index+1, err)
		}
	}
	header := parsed[0]
	if header.Type != "session" || header.Version != 3 || header.ID == "" || header.Timestamp == "" || header.CWD == "" {
		t.Fatalf("OMP header = %#v", header)
	}
	for index, record := range parsed[1:] {
		if record.ID == "" || record.Timestamp == "" {
			t.Fatalf("OMP entry %d = %#v", index+2, record)
		}
		switch record.Type {
		case "message", "compaction", "custom":
		default:
			t.Fatalf("OMP entry %d type = %q", index+2, record.Type)
		}
	}
	base := sessionio.FileLocator{Root: fixtureRoot, Path: "omp/session.jsonl"}
	occurrence := sessionio.SourceOccurrence{
		ID: "omp-occurrence-session-01", SourceID: "omp-source", Harness: sessionio.HarnessOMP,
		Locator: sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &base},
	}
	session := sessionio.SessionRef{
		ID: "omp-session-session-01", NativeID: header.ID, Title: header.Title,
		DiscoveryRevision: "omp-discovery-v3",
		Native: sessionio.NativeSessionMetadata{Identities: []sessionio.NativeIdentity{{
			Kind: sessionio.NativeIdentityKindSession, Value: header.ID,
		}}},
		Occurrence: occurrence,
	}
	count := map[string]int{}
	for _, record := range parsed[1:] {
		if record.ID != "" {
			count[record.ID]++
		}
	}

	items := make([]sessionio.ReadItem, 0, len(records))
	for index, record := range parsed {
		observationID := sessionio.ObservationID("omp-observation-" + record.ID)
		locator := records[index].SourceLocator(base)
		item := sessionio.ReadItem{
			Session: session,
			Observation: sessionio.NativeObservation{
				ID: observationID, NativeKind: record.Type, NativeVersion: "omp/v3",
				Locator: locator, Revision: revision, Representation: records[index].NativeRepresentation(),
			},
		}
		if index > 0 {
			item.Events, item.Observation.Limitations, item.Diagnostics = ompEvents(observationID, locator, record)
			parentID, parentErr := ompParentID(record.ParentID)
			if parentErr != nil {
				item.Diagnostics = append(item.Diagnostics, diagnostic("omp_malformed_parent", parentErr.Error(), locator))
			} else if parentID != "" {
				switch count[parentID] {
				case 1:
					item.Relations = append(item.Relations, relation(
						"omp-reply-"+record.ID, sessionio.RelationKindReplyTo,
						sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(observationID)},
						sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: "omp-observation-" + parentID},
						sessionio.RelationOriginNative, observationID, locator,
					))
				case 0:
					item.Diagnostics = append(item.Diagnostics, diagnostic("omp_missing_parent", "OMP parentId has no native entry target", locator))
				default:
					item.Diagnostics = append(item.Diagnostics, diagnostic("omp_ambiguous_parent", "OMP parentId has multiple native entry targets", locator))
				}
			}
		}
		items = append(items, item)
	}
	if len(parsed) > 1 {
		leaf := &items[len(items)-1]
		leaf.Relations = append(leaf.Relations, relation(
			"omp-active-leaf", sessionio.RelationKindActiveLeaf,
			sessionio.NodeRef{Kind: sessionio.NodeKindSession, ID: string(session.ID)},
			sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(leaf.Observation.ID)},
			sessionio.RelationOriginDeterministic, leaf.Observation.ID, leaf.Observation.Locator,
		))
	}
	return session, items
}

func ompEvents(id sessionio.ObservationID, locator sessionio.SourceLocator, record ompRecord) ([]sessionio.Event, []sessionio.SourceLimitation, []sessionio.Diagnostic) {
	evidence := []sessionio.EvidenceRef{{Observation: id, Locator: locator}}
	switch record.Type {
	case "compaction":
		if record.Summary == "" || record.FirstKeptEntryID == "" || record.TokensBefore <= 0 {
			return nil, nil, []sessionio.Diagnostic{diagnostic("omp_malformed_compaction", "OMP compaction is missing required persisted fields", locator)}
		}
		return []sessionio.Event{{ID: sessionio.EventID("omp-event-" + record.ID), Kind: sessionio.EventKindMarker, Evidence: evidence,
			Marker: &sessionio.MarkerEvent{Name: "compaction", State: record.Summary}}}, nil, nil
	case "custom":
		if record.CustomType == "" || len(record.Data) == 0 {
			return nil, nil, []sessionio.Diagnostic{diagnostic("omp_malformed_custom", "OMP custom entry is missing required persisted fields", locator)}
		}
		return []sessionio.Event{{ID: sessionio.EventID("omp-event-" + record.ID), Kind: sessionio.EventKindUnknown, Evidence: evidence,
			Unknown: &sessionio.UnknownEvent{NativeType: record.CustomType}}}, nil, nil
	case "message":
		if record.Message == nil || record.Message.Role == "" {
			return nil, nil, []sessionio.Diagnostic{diagnostic("omp_malformed_message", "OMP message is missing required persisted fields", locator)}
		}
	default:
		return nil, nil, []sessionio.Diagnostic{diagnostic("omp_unknown_entry", "OMP entry type is not projected", locator)}
	}

	role := ompRole(record.Message.Role)
	if role == sessionio.MessageRoleUnknown {
		return nil, nil, []sessionio.Diagnostic{diagnostic("omp_malformed_message", "OMP message role is unsupported", locator)}
	}
	var events []sessionio.Event
	var blocks []sessionio.ContentBlock
	var limits []sessionio.SourceLimitation
	var diagnostics []sessionio.Diagnostic
	for index, raw := range record.Message.Content {
		var content ompContentBlock
		if err := json.Unmarshal(raw, &content); err != nil || content.Type == "" {
			diagnostics = append(diagnostics, diagnostic("omp_malformed_content", "OMP message content has an invalid persisted shape", locator))
			continue
		}
		contentID := sessionio.ContentID(fmt.Sprintf("omp-content-%s-%d", record.ID, index))
		switch content.Type {
		case "text":
			if content.Text == nil {
				diagnostics = append(diagnostics, diagnostic("omp_malformed_content", "OMP text content is missing text", locator))
				continue
			}
			blocks = append(blocks, sessionio.ContentBlock{ID: contentID, Kind: sessionio.ContentKindText, Availability: sessionio.ContentAvailabilityAvailable, Text: &sessionio.TextContent{Text: *content.Text}})
		case "image":
			availability := sessionio.ContentAvailabilityExternal
			if !verifiedOMPBlob(content.Data) {
				availability = sessionio.ContentAvailabilityUnavailable
				limits = append(limits, sessionio.SourceLimitation{Kind: sessionio.LimitationKindMissingExternalPayload, Detail: "referenced synthetic blob is absent or invalid"})
			} else {
				limits = append(limits, sessionio.SourceLimitation{Kind: sessionio.LimitationKindExternalPayload, Detail: "verified external synthetic blob is not imported"})
			}
			blocks = append(blocks, sessionio.ContentBlock{ID: contentID, Kind: sessionio.ContentKindMedia, Availability: availability, Media: &sessionio.MediaContent{MediaType: content.MIMEType, Reference: content.Data}})
		case "toolCall":
			if content.ID == "" || content.Name == "" || len(content.Arguments) == 0 {
				diagnostics = append(diagnostics, diagnostic("omp_malformed_tool_call", "OMP toolCall content is missing required fields", locator))
				continue
			}
			events = append(events, sessionio.Event{ID: sessionio.EventID(fmt.Sprintf("omp-tool-call-%s-%d", record.ID, index)), Kind: sessionio.EventKindToolCall, Evidence: evidence,
				ToolCall: &sessionio.ToolCallEvent{CallID: content.ID, Name: content.Name, Input: sessionio.Payload{MediaType: "application/json", Data: content.Arguments}}})
		default:
			blocks = append(blocks, sessionio.ContentBlock{ID: contentID, Kind: sessionio.ContentKindOpaque, Availability: sessionio.ContentAvailabilityAvailable, Opaque: &sessionio.OpaqueContent{NativeType: "omp/" + content.Type, MediaType: "application/json", Data: raw}})
		}
	}
	if len(blocks) != 0 {
		events = append(events, sessionio.Event{ID: sessionio.EventID("omp-message-" + record.ID), Kind: sessionio.EventKindMessage, Evidence: evidence,
			Message: &sessionio.MessageEvent{Role: role, Content: blocks}})
	}
	if record.Message.Role == "toolResult" {
		if record.Message.ToolCallID == "" || record.Message.ToolName == "" || record.Message.IsError == nil {
			diagnostics = append(diagnostics, diagnostic("omp_malformed_tool_result", "OMP toolResult is missing required fields", locator))
		} else {
			status := sessionio.ToolResultStatusSuccess
			if *record.Message.IsError {
				status = sessionio.ToolResultStatusError
			}
			output, _ := json.Marshal(record.Message.Content)
			events = append(events, sessionio.Event{ID: sessionio.EventID("omp-tool-result-" + record.ID), Kind: sessionio.EventKindToolResult, Evidence: evidence,
				ToolResult: &sessionio.ToolResultEvent{CallID: record.Message.ToolCallID, Status: status, Output: sessionio.Payload{MediaType: "application/json", Data: output}}})
		}
	}
	return events, limits, diagnostics
}

func ompRole(role string) sessionio.MessageRole {
	switch role {
	case "user":
		return sessionio.MessageRoleUser
	case "assistant":
		return sessionio.MessageRoleAssistant
	case "toolResult":
		return sessionio.MessageRoleTool
	default:
		return sessionio.MessageRoleUnknown
	}
}

func ompParentID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var parentID string
	if err := json.Unmarshal(raw, &parentID); err != nil || parentID == "" {
		return "", fmt.Errorf("OMP parentId must be a non-empty string")
	}
	return parentID, nil
}

func verifiedOMPBlob(reference string) bool {
	const prefix = "blob:sha256:"
	if !strings.HasPrefix(reference, prefix) {
		return false
	}
	digest := strings.TrimPrefix(reference, prefix)
	if len(digest) != sha256.Size*2 {
		return false
	}
	data, err := os.ReadFile(fixturePath(filepath.Join("omp-blobs", digest+".blob")))
	if err != nil {
		return false
	}
	actual := sha256.Sum256(data)
	return hex.EncodeToString(actual[:]) == digest
}

func TestOpenCodeContract(t *testing.T) {
	manifest := loadManifest(t)
	manifestBytes, err := os.ReadFile(fixturePath("opencode-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifestBytes, []byte(`"order"`)) {
		t.Fatal("manifest must not carry a sidecar ordering field; ordering comes from native row cells")
	}
	verifyManifestEvidence(t, manifest)
	if err := validateSchema(manifest.RequiredSchema, manifest.SchemaVariants["additive"]); err != nil {
		t.Fatalf("additive schema rejected: %v", err)
	}
	err = validateSchema(manifest.RequiredSchema, manifest.SchemaVariants["incompatible"])
	if err == nil || !strings.Contains(err.Error(), "database:opencode.db:session_message") ||
		!strings.Contains(err.Error(), "required column \"data\"") {
		t.Fatalf("incompatible schema error = %v", err)
	}

	session, items, err := projectOpenCode(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var databaseRows, eventRows, relations int
	var typed, materialized, unknownColumn, opaquePart bool
	var messages, parts []string
	for _, item := range items {
		switch item.Observation.Revision.Kind {
		case sessionio.RevisionKindDatabaseTransaction:
			databaseRows++
			if item.Observation.Revision.Value != manifest.TransactionRevision ||
				item.Observation.Representation.Capture != sessionio.CaptureKindStructuredSnapshot {
				t.Fatalf("database observation = %#v", item.Observation)
			}
			if len(item.Observation.Limitations) != 1 ||
				item.Observation.Limitations[0].Kind != sessionio.LimitationKindMutableMaterialization {
				t.Fatalf("database limitation = %#v", item.Observation.Limitations)
			}
			materialized = true
			typed = typed || (bytes.Contains(item.Observation.Representation.Data, []byte("\"type\":\"NULL\"")) &&
				bytes.Contains(item.Observation.Representation.Data, []byte("\"type\":\"INTEGER\"")) &&
				bytes.Contains(item.Observation.Representation.Data, []byte("\"type\":\"REAL\"")) &&
				bytes.Contains(item.Observation.Representation.Data, []byte("\"type\":\"TEXT\"")) &&
				bytes.Contains(item.Observation.Representation.Data, []byte("\"type\":\"BLOB\"")))
			if item.Observation.Locator.Database.Table == "part" {
				parts = append(parts, item.Observation.Locator.Database.Keys[0].Value)
			}
			if item.Observation.Locator.Database.Table == "message" {
				messages = append(messages, item.Observation.Locator.Database.Keys[0].Value)
			}
			assertNativeKeys(t, item.Observation.Locator)
			unknownColumn = unknownColumn || bytes.Contains(item.Observation.Representation.Data, []byte("future_part_column"))
		case sessionio.RevisionKindEventSequence:
			eventRows++
			if item.Observation.Revision.Value != manifest.EventRevision {
				t.Fatalf("event revision = %#v", item.Observation.Revision)
			}
			if item.Observation.Locator.Kind != sessionio.LocatorKindFile ||
				item.Observation.Locator.File == nil ||
				item.Observation.Locator.File.Path != manifest.Database.EventPath ||
				item.Observation.Locator.File.Line == nil ||
				item.Observation.Locator.File.ByteRange == nil {
				t.Fatalf("event locator = %#v", item.Observation.Locator)
			}
		}
		for _, relation := range item.Relations {
			if relation.Kind == sessionio.RelationKindMaterializes || relation.Kind == sessionio.RelationKindUpdates {
				relations++
				if relation.Origin != sessionio.RelationOriginNative || len(relation.Evidence) != 1 {
					t.Fatalf("materialization relation = %#v", relation)
				}
			}
		}
		for _, event := range item.Events {
			opaquePart = opaquePart || (event.Kind == sessionio.EventKindUnknown && event.Unknown != nil && event.Unknown.NativeType == "opencode/future-rich-part")
		}
	}
	if databaseRows != len(manifest.Rows) || eventRows != len(manifest.Events) || relations != 2 ||
		!typed || !materialized || !unknownColumn || !opaquePart {
		t.Fatalf("rows=%d events=%d relations=%d typed=%t materialized=%t unknown_column=%t opaque_part=%t", databaseRows, eventRows, relations, typed, materialized, unknownColumn, opaquePart)
	}
	if strings.Join(messages, ",") != "message-01-user,message-02-assistant" {
		t.Fatalf("message order = %v", messages)
	}
	// Part ordering is derived from the captured native row cells: (message_id, id).
	// The user message's id sorts before the assistant message's id.
	wantParts := "part-user-01-text,part-assistant-01-reasoning,part-assistant-02-tool,part-assistant-03-file,part-assistant-04-future"
	if strings.Join(parts, ",") != wantParts {
		t.Fatalf("part order = %v, want %s", parts, wantParts)
	}
	assertRecordsValidate(t, session, items)
}

func TestOpenCodeRejectsInconsistentSnapshots(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*manifest)
		want   string
	}{
		{
			name: "duplicate native ordering key",
			mutate: func(value *manifest) {
				value.Rows[5].Cells[0][2] = json.RawMessage(`"part-assistant-01-reasoning"`)
				value.Rows[5].Keys[0][1] = "part-assistant-01-reasoning"
			},
			want: "database:opencode.db:part,id=part-assistant-01-reasoning: duplicate native ordering key",
		},
		{
			name:   "transaction changed during acquisition",
			mutate: func(value *manifest) { value.Rows[1].TransactionRevision = "sqlite-tx:changed" },
			want:   "database:opencode.db:message,id=message-01-user: transaction revision changed during acquisition",
		},
		{
			name: "incompatible known JSON shape",
			mutate: func(value *manifest) {
				value.Rows[3].Cells[5][2] = json.RawMessage(`"{\"type\":\"text\"}"`)
			},
			want: "database:opencode.db:part,id=part-user-01-text: known text part is missing text",
		},
		{
			name: "reasoning missing time",
			mutate: func(value *manifest) {
				value.Rows[4].Cells[5][2] = json.RawMessage(`"{\"type\":\"reasoning\",\"text\":\"missing time\"}"`)
			},
			want: "database:opencode.db:part,id=part-assistant-01-reasoning: known reasoning part is missing text or time.start",
		},
		{
			name: "completed tool missing title and metadata",
			mutate: func(value *manifest) {
				value.Rows[5].Cells[5][2] = json.RawMessage(`"{\"type\":\"tool\",\"callID\":\"tool-01\",\"tool\":\"synthetic.tool\",\"state\":{\"status\":\"completed\",\"input\":{},\"output\":\"done\",\"time\":{\"start\":1,\"end\":2}}}"`)
			},
			want: "database:opencode.db:part,id=part-assistant-02-tool: known completed tool part is missing input, output, or title",
		},
		{
			name: "incomplete message info",
			mutate: func(value *manifest) {
				value.Rows[1].Cells[4][2] = json.RawMessage(`"{\"id\":\"message-01-user\",\"role\":\"user\"}"`)
			},
			want: "database:opencode.db:message,id=message-01-user: stored message data must not contain identity field \"id\"",
		},
		{
			name: "completed tool missing metadata",
			mutate: func(value *manifest) {
				value.Rows[5].Cells[5][2] = json.RawMessage(`"{\"type\":\"tool\",\"callID\":\"tool-01\",\"tool\":\"synthetic.tool\",\"state\":{\"status\":\"completed\",\"input\":{},\"output\":\"done\",\"title\":\"Synthetic tool\",\"time\":{\"start\":1,\"end\":2}}}"`)
			},
			want: "database:opencode.db:part,id=part-assistant-02-tool: known completed tool part is missing metadata",
		},
		{
			name: "completed tool input is not an object",
			mutate: func(value *manifest) {
				value.Rows[5].Cells[5][2] = json.RawMessage(`"{\"type\":\"tool\",\"callID\":\"tool-01\",\"tool\":\"synthetic.tool\",\"state\":{\"status\":\"completed\",\"input\":\"invalid\",\"output\":\"done\",\"title\":\"Synthetic tool\",\"metadata\":{},\"time\":{\"start\":1,\"end\":2}}}"`)
			},
			want: "database:opencode.db:part,id=part-assistant-02-tool: known completed tool part is missing input, output, or title",
		},
		{
			name: "assistant missing agent",
			mutate: func(value *manifest) {
				value.Rows[2].Cells[4][2] = json.RawMessage(`"{\"role\":\"assistant\",\"time\":{\"created\":1700000002},\"parentID\":\"message-01-user\",\"modelID\":\"synthetic-model\",\"providerID\":\"synthetic-provider\",\"mode\":\"build\",\"path\":{\"cwd\":\"/synthetic\",\"root\":\"/synthetic\"},\"cost\":0.01,\"tokens\":{\"input\":10,\"output\":20,\"reasoning\":5,\"cache\":{\"read\":0,\"write\":0}}}"`)
			},
			want: "database:opencode.db:message,id=message-02-assistant: known assistant message is missing agent",
		},
		{
			name:   "locator key diverges from typed row cell",
			mutate: func(value *manifest) { value.Rows[4].Cells[0][2] = json.RawMessage(`"part-mismatch"`) },
			want:   "database:opencode.db:part,id=part-assistant-01-reasoning: locator key \"id\" does not match typed row cell",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cloneManifest(t, loadManifest(t))
			test.mutate(&value)
			if _, _, err := projectOpenCode(value); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("projectOpenCode() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOpenCodeRejectsEventTargetDivergence(t *testing.T) {
	value := cloneManifest(t, loadManifest(t))
	value.Events[0].Keys[0][1] = "part-assistant-03-file"
	if _, _, err := projectOpenCode(value); err == nil || !strings.Contains(err.Error(), "opencode event:1: raw event target does not match manifest target keys") {
		t.Fatalf("projectOpenCode() error = %v", err)
	}
}

func TestContractStressDoesNotRegisterAdapters(t *testing.T) {
	registry, err := sessionio.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, harness := range []sessionio.Harness{sessionio.HarnessOMP, sessionio.HarnessOpenCode} {
		if _, found := registry.Adapter(harness); found {
			t.Fatalf("unexpected registered %s adapter", harness)
		}
	}
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	data, err := os.ReadFile(fixturePath("opencode-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result manifest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func verifyManifestEvidence(t *testing.T, manifest manifest) {
	t.Helper()
	for _, item := range []struct{ path, digest string }{
		{manifest.Database.Path, manifest.Database.SHA256},
		{manifest.Database.WALPath, manifest.Database.WALSHA256},
		{manifest.Database.EventPath, manifest.Database.EventSHA256},
	} {
		data, err := os.ReadFile(fixturePath(item.path))
		if err != nil {
			t.Fatal(err)
		}
		actual := sha256.Sum256(data)
		if hex.EncodeToString(actual[:]) != item.digest {
			t.Fatalf("%s digest = %x, want %s", item.path, actual, item.digest)
		}
	}
	database, err := os.ReadFile(fixturePath(manifest.Database.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(database, []byte("SQLite format 3\x00")) {
		t.Fatalf("SQLite header = %x", database[:min(len(database), 16)])
	}
	wal, err := os.ReadFile(fixturePath(manifest.Database.WALPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(wal) < 4 || !bytes.Equal(wal[:4], []byte{0x37, 0x7f, 0x06, 0x82}) {
		t.Fatalf("WAL header = %x", wal[:min(len(wal), 4)])
	}
}

func assertNativeKeys(t *testing.T, locator sessionio.SourceLocator) {
	t.Helper()
	if locator.Database == nil {
		t.Fatal("database observation has no database locator")
	}
	var want []string
	switch locator.Database.Table {
	case "session", "message", "part", "session_message":
		want = []string{"id"}
	default:
		t.Fatalf("unexpected database table %q", locator.Database.Table)
	}
	if len(locator.Database.Keys) != len(want) {
		t.Fatalf("database keys = %#v, want %v", locator.Database.Keys, want)
	}
	for index, key := range locator.Database.Keys {
		if key.Name != want[index] || key.Value == "" {
			t.Fatalf("database keys = %#v, want %v", locator.Database.Keys, want)
		}
	}
}

func cloneManifest(t *testing.T, value manifest) manifest {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result manifest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func validateSchema(required, actual map[string][]string) error {
	for table, wanted := range required {
		available := map[string]struct{}{}
		for _, column := range actual[table] {
			available[column] = struct{}{}
		}
		for _, column := range wanted {
			if _, found := available[column]; !found {
				return fmt.Errorf("opencode database:opencode.db:%s: required column %q is missing", table, column)
			}
		}
	}
	return nil
}

func projectOpenCode(manifest manifest) (sessionio.SessionRef, []sessionio.ReadItem, error) {
	if err := validateSchema(manifest.RequiredSchema, manifest.SchemaVariants["additive"]); err != nil {
		return sessionio.SessionRef{}, nil, err
	}
	type orderedRow struct {
		row     manifestRow
		key     string
		rank    int
		locator sessionio.SourceLocator
	}
	rows := make([]orderedRow, 0, len(manifest.Rows))
	seenOrders := map[string]struct{}{}
	for _, row := range manifest.Rows {
		locator, err := manifestRowLocator(manifest, row)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		revision := manifest.TransactionRevision
		if row.TransactionRevision != "" {
			revision = row.TransactionRevision
		}
		if revision != manifest.TransactionRevision {
			return sessionio.SessionRef{}, nil, fmt.Errorf(
				"opencode %s: transaction revision changed during acquisition",
				databaseContext(locator),
			)
		}
		if err := validateLocatorKeys(locator, row); err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		rank, key, err := nativeOrderKey(locator, row)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		identity := row.Table + "|" + key
		if _, found := seenOrders[identity]; found {
			return sessionio.SessionRef{}, nil, fmt.Errorf(
				"opencode %s: duplicate native ordering key",
				databaseContext(locator),
			)
		}
		seenOrders[identity] = struct{}{}
		rows = append(rows, orderedRow{row: row, key: key, rank: rank, locator: locator})
	}
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].rank != rows[right].rank {
			return rows[left].rank < rows[right].rank
		}
		return rows[left].key < rows[right].key
	})
	source := databaseLocator(manifest.Database.Path, "session", nil)
	session := sessionio.SessionRef{
		ID: "opencode-session-synthetic", NativeID: "oc-session-01", Title: "Synthetic OpenCode",
		DiscoveryRevision: "opencode-discovery-synthetic",
		Native: sessionio.NativeSessionMetadata{Identities: []sessionio.NativeIdentity{{
			Kind: sessionio.NativeIdentityKindSession, Value: "oc-session-01",
		}}},
		Occurrence: sessionio.SourceOccurrence{
			ID: "opencode-occurrence-synthetic", SourceID: "opencode-source", Harness: sessionio.HarnessOpenCode, Locator: source,
		},
	}
	items := make([]sessionio.ReadItem, 0, len(rows)+len(manifest.Events))
	rowIDs := map[string]sessionio.ObservationID{}
	rowsByIdentity := map[string]manifestRow{}
	for _, ordered := range rows {
		row := ordered.row
		keys, err := manifestKeys(row.Keys)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		locator := ordered.locator
		data, err := structuredRow(locator, row)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		id := sessionio.ObservationID("opencode-row-" + row.Table + "-" + strings.Join(keyValues(keys), "-"))
		rowIDs[row.Table+"|"+keyIdentity(keys)] = id
		rowsByIdentity[row.Table+"|"+keyIdentity(keys)] = row
		events, err := openCodeEvents(id, locator, row)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		items = append(items, sessionio.ReadItem{
			Session: session,
			Observation: sessionio.NativeObservation{
				ID: id, NativeKind: row.NativeKind, Locator: locator,
				Revision: sessionio.Revision{Kind: sessionio.RevisionKindDatabaseTransaction, Value: manifest.TransactionRevision},
				Representation: sessionio.NativeRepresentation{Capture: sessionio.CaptureKindStructuredSnapshot,
					MediaType: "application/vnd.sqlite.row+json", Data: data},
				Limitations: []sessionio.SourceLimitation{{Kind: sessionio.LimitationKindMutableMaterialization,
					Detail: "offline database rows are current materializations"}},
			},
			Events: events,
		})
	}
	lines, err := eventLines(manifest.Database.EventPath)
	if err != nil {
		return sessionio.SessionRef{}, nil, err
	}
	for _, event := range manifest.Events {
		keys, err := manifestKeys(event.Keys)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		line, found := lines[event.Sequence]
		if !found {
			return sessionio.SessionRef{}, nil, fmt.Errorf("opencode event:%d is missing", event.Sequence)
		}
		projection, err := parseOpenCodeEvent(line.Data, event.Sequence)
		if err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		if projection.table != event.Table || len(keys) != 1 || keys[0].Name != "id" || keys[0].Value != projection.id {
			return sessionio.SessionRef{}, nil, fmt.Errorf("opencode event:%d: raw event target does not match manifest target keys", event.Sequence)
		}
		identity := event.Table + "|" + keyIdentity(keys)
		target := rowIDs[identity]
		if target == "" {
			return sessionio.SessionRef{}, nil, fmt.Errorf("opencode database:%s:%s: event target is missing", manifest.Database.Path, event.Table)
		}
		if err := validateEventTarget(event.Sequence, projection, rowsByIdentity[identity], manifest); err != nil {
			return sessionio.SessionRef{}, nil, err
		}
		sequence := fmt.Sprintf("%d", event.Sequence)
		id := sessionio.ObservationID("opencode-event-" + sequence)
		locator := line.Locator
		items = append(items, sessionio.ReadItem{
			Session: session,
			Observation: sessionio.NativeObservation{
				ID: id, NativeKind: projection.nativeKind, Locator: locator,
				Revision: sessionio.Revision{Kind: sessionio.RevisionKindEventSequence, Value: manifest.EventRevision},
				Representation: sessionio.NativeRepresentation{Capture: sessionio.CaptureKindByteExact,
					MediaType: "application/json", Data: line.Data, Framing: line.Framing},
			},
			Events: []sessionio.Event{{ID: sessionio.EventID("opencode-event-projection-" + sequence),
				Kind: sessionio.EventKindMarker, Evidence: []sessionio.EvidenceRef{{Observation: id, Locator: locator}},
				Marker: &sessionio.MarkerEvent{Name: projection.nativeKind}}},
			Relations: []sessionio.Relation{relation("opencode-event-relation-"+sequence, projection.relation,
				sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(id)},
				sessionio.NodeRef{Kind: sessionio.NodeKindObservation, ID: string(target)},
				sessionio.RelationOriginNative, id, locator)},
		})
	}
	return session, items, nil
}

func nativeOrderKey(locator sessionio.SourceLocator, row manifestRow) (int, string, error) {
	id, err := requiredTextCell(locator, row, "id")
	if err != nil {
		return 0, "", err
	}
	switch row.Table {
	case "session":
		return 0, id, nil
	case "message", "session_message":
		timeCreated, err := requiredIntegerCell(locator, row, "time_created")
		if err != nil {
			return 0, "", err
		}
		rank := 1
		if row.Table == "session_message" {
			rank = 3
		}
		return rank, fmt.Sprintf("%020d\x00%s", timeCreated, id), nil
	case "part":
		messageID, err := requiredTextCell(locator, row, "message_id")
		if err != nil {
			return 0, "", err
		}
		return 2, messageID + "\x00" + id, nil
	default:
		return 0, "", fmt.Errorf("opencode %s: unsupported native table", databaseContext(locator))
	}
}

func manifestKeys(values [][]string) ([]sessionio.DatabaseKey, error) {
	keys := make([]sessionio.DatabaseKey, len(values))
	for index, value := range values {
		if len(value) != 2 || value[0] == "" {
			return nil, fmt.Errorf("opencode manifest key %d is invalid", index)
		}
		keys[index] = sessionio.DatabaseKey{Name: value[0], Value: value[1]}
	}
	return keys, nil
}

func manifestRowLocator(manifest manifest, row manifestRow) (sessionio.SourceLocator, error) {
	keys, err := manifestKeys(row.Keys)
	if err != nil {
		return sessionio.SourceLocator{}, err
	}
	return databaseLocator(manifest.Database.Path, row.Table, keys), nil
}

func databaseContext(locator sessionio.SourceLocator) string {
	if locator.Database == nil {
		return "database"
	}
	parts := []string{"database:" + locator.Database.Path + ":" + locator.Database.Table}
	for _, key := range locator.Database.Keys {
		parts = append(parts, key.Name+"="+key.Value)
	}
	return strings.Join(parts, ",")
}

func validateLocatorKeys(locator sessionio.SourceLocator, row manifestRow) error {
	if locator.Database == nil {
		return fmt.Errorf("opencode database locator is missing")
	}
	for _, key := range locator.Database.Keys {
		value, err := requiredTextCell(locator, row, key.Name)
		if err != nil {
			return err
		}
		if value != key.Value {
			return fmt.Errorf(
				"opencode %s: locator key %q does not match typed row cell",
				databaseContext(locator),
				key.Name,
			)
		}
	}
	return nil
}

func structuredRow(locator sessionio.SourceLocator, row manifestRow) ([]byte, error) {
	type cell struct {
		Name  string          `json:"name"`
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	cells := make([]cell, len(row.Cells))
	for index, value := range row.Cells {
		if len(value) != 3 {
			return nil, fmt.Errorf("opencode %s: row cell %d is invalid", databaseContext(locator), index)
		}
		if err := json.Unmarshal(value[0], &cells[index].Name); err != nil {
			return nil, fmt.Errorf("opencode %s: cell %d name: %w", databaseContext(locator), index, err)
		}
		if err := json.Unmarshal(value[1], &cells[index].Type); err != nil {
			return nil, fmt.Errorf("opencode %s: cell %q type: %w", databaseContext(locator), cells[index].Name, err)
		}
		cells[index].Value = value[2]
		switch cells[index].Type {
		case "NULL":
			if string(cells[index].Value) != "null" {
				return nil, fmt.Errorf("opencode %s: known SQLite NULL value must be JSON null", databaseContext(locator))
			}
		case "INTEGER":
			var number json.Number
			if err := json.Unmarshal(cells[index].Value, &number); err != nil {
				return nil, fmt.Errorf("opencode %s: known SQLite INTEGER value must be a JSON number", databaseContext(locator))
			}
			if _, err := number.Int64(); err != nil {
				return nil, fmt.Errorf("opencode %s: known SQLite INTEGER value must be an integer", databaseContext(locator))
			}
		case "REAL":
			var number json.Number
			if err := json.Unmarshal(cells[index].Value, &number); err != nil {
				return nil, fmt.Errorf("opencode %s: known SQLite REAL value must be a JSON number", databaseContext(locator))
			}
			if _, err := number.Float64(); err != nil {
				return nil, fmt.Errorf("opencode %s: known SQLite REAL value must be a JSON number", databaseContext(locator))
			}
		case "TEXT", "BLOB":
			var text string
			if err := json.Unmarshal(cells[index].Value, &text); err != nil {
				return nil, fmt.Errorf("opencode %s: known SQLite %s value must be a JSON string", databaseContext(locator), cells[index].Type)
			}
		default:
			return nil, fmt.Errorf("opencode %s: cell %q has unsupported SQLite type %q", databaseContext(locator), cells[index].Name, cells[index].Type)
		}
	}
	return json.Marshal(struct {
		Table string `json:"table"`
		Cells []cell `json:"cells"`
	}{Table: row.Table, Cells: cells})
}

type openCodeMessage struct {
	ID        string
	SessionID string
	Role      string
}

type openCodePart struct {
	ID        string
	SessionID string
	MessageID string
	Type      string
	Text      string
	Input     json.RawMessage
	MIME      string
	URL       string
	CallID    string
	Tool      string
}

func openCodeEvents(id sessionio.ObservationID, locator sessionio.SourceLocator, row manifestRow) ([]sessionio.Event, error) {
	context := databaseContext(locator)
	if row.Table == "message" {
		data, err := requiredTextCell(locator, row, "data")
		if err != nil {
			return nil, err
		}
		hydrated, err := hydrateOpenCodeMessage(locator, row, []byte(data))
		if err != nil {
			return nil, err
		}
		message, err := parseOpenCodeMessage(hydrated, context)
		if err != nil {
			return nil, err
		}
		if err := validateMessageRow(locator, row, message); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if row.Table != "part" {
		return nil, nil
	}
	data, err := requiredTextCell(locator, row, "data")
	if err != nil {
		return nil, err
	}
	hydrated, err := hydrateOpenCodePart(locator, row, []byte(data))
	if err != nil {
		return nil, err
	}
	part, err := parseOpenCodePart(hydrated, context)
	if err != nil {
		return nil, err
	}
	if err := validatePartRow(locator, row, part); err != nil {
		return nil, err
	}
	evidence := []sessionio.EvidenceRef{{Observation: id, Locator: locator}}
	eventID := sessionio.EventID("opencode-projection-" + string(id))
	textBlock := func(text string) sessionio.ContentBlock {
		return sessionio.ContentBlock{ID: sessionio.ContentID("opencode-content-" + string(id)),
			Kind: sessionio.ContentKindText, Availability: sessionio.ContentAvailabilityAvailable,
			Text: &sessionio.TextContent{Text: text}}
	}
	switch part.Type {
	case "text":
		return []sessionio.Event{{ID: eventID, Kind: sessionio.EventKindMessage, Evidence: evidence,
			Message: &sessionio.MessageEvent{Role: sessionio.MessageRoleUnknown, Content: []sessionio.ContentBlock{textBlock(part.Text)}}}}, nil
	case "reasoning":
		return []sessionio.Event{{ID: eventID, Kind: sessionio.EventKindReasoning, Evidence: evidence,
			Reasoning: &sessionio.ReasoningEvent{Content: []sessionio.ContentBlock{textBlock(part.Text)}}}}, nil
	case "tool":
		return []sessionio.Event{{ID: eventID, Kind: sessionio.EventKindToolCall, Evidence: evidence,
			ToolCall: &sessionio.ToolCallEvent{CallID: part.CallID, Name: part.Tool,
				Input: sessionio.Payload{MediaType: "application/json", Data: part.Input}}}}, nil
	case "file":
		return []sessionio.Event{{ID: eventID, Kind: sessionio.EventKindMessage, Evidence: evidence,
			Message: &sessionio.MessageEvent{Role: sessionio.MessageRoleAssistant, Content: []sessionio.ContentBlock{{
				ID: sessionio.ContentID("opencode-content-" + string(id)), Kind: sessionio.ContentKindMedia,
				Availability: sessionio.ContentAvailabilityExternal, Media: &sessionio.MediaContent{MediaType: part.MIME, Reference: part.URL},
			}}}}}, nil
	default:
		return []sessionio.Event{{ID: eventID, Kind: sessionio.EventKindUnknown, Evidence: evidence,
			Unknown: &sessionio.UnknownEvent{NativeType: "opencode/" + part.Type}}}, nil
	}
}

func hydrateOpenCodeMessage(locator sessionio.SourceLocator, row manifestRow, data []byte) ([]byte, error) {
	object, err := jsonObject(data, databaseContext(locator), "stored message data")
	if err != nil {
		return nil, err
	}
	if _, found := object["id"]; found {
		return nil, fmt.Errorf("opencode %s: stored message data must not contain identity field \"id\"", databaseContext(locator))
	}
	if _, found := object["sessionID"]; found {
		return nil, fmt.Errorf("opencode %s: stored message data must not contain identity field \"sessionID\"", databaseContext(locator))
	}
	id, err := requiredTextCell(locator, row, "id")
	if err != nil {
		return nil, err
	}
	sessionID, err := requiredTextCell(locator, row, "session_id")
	if err != nil {
		return nil, err
	}
	object["id"], _ = json.Marshal(id)
	object["sessionID"], _ = json.Marshal(sessionID)
	return json.Marshal(object)
}

func hydrateOpenCodePart(locator sessionio.SourceLocator, row manifestRow, data []byte) ([]byte, error) {
	object, err := jsonObject(data, databaseContext(locator), "stored part data")
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"id", "sessionID", "messageID"} {
		if _, found := object[field]; found {
			return nil, fmt.Errorf("opencode %s: stored part data must not contain identity field %q", databaseContext(locator), field)
		}
	}
	for field, column := range map[string]string{"id": "id", "sessionID": "session_id", "messageID": "message_id"} {
		value, err := requiredTextCell(locator, row, column)
		if err != nil {
			return nil, err
		}
		object[field], _ = json.Marshal(value)
	}
	return json.Marshal(object)
}

func parseOpenCodeMessage(data []byte, context string) (openCodeMessage, error) {
	object, err := jsonObject(data, context, "message data")
	if err != nil {
		return openCodeMessage{}, err
	}
	message := openCodeMessage{
		ID:        requiredJSONString(object, context, "message id", "id"),
		SessionID: requiredJSONString(object, context, "message sessionID", "sessionID"),
		Role:      requiredJSONString(object, context, "message role", "role"),
	}
	if message.ID == "" || message.SessionID == "" || message.Role == "" {
		return openCodeMessage{}, fmt.Errorf("opencode %s: known message data is missing required fields", context)
	}
	time, err := requiredJSONObject(object, context, "message time", "time")
	if err != nil || !hasJSONNumber(time, "created") {
		return openCodeMessage{}, fmt.Errorf("opencode %s: known message data is missing time.created", context)
	}
	switch message.Role {
	case "user":
		if requiredJSONString(object, context, "message agent", "agent") == "" {
			return openCodeMessage{}, fmt.Errorf("opencode %s: known user message is missing agent", context)
		}
		model, err := requiredJSONObject(object, context, "message model", "model")
		if err != nil || requiredJSONString(model, context, "message model providerID", "providerID") == "" || requiredJSONString(model, context, "message model modelID", "modelID") == "" {
			return openCodeMessage{}, fmt.Errorf("opencode %s: known user message has incomplete model", context)
		}
	case "assistant":
		for _, field := range []string{"parentID", "modelID", "providerID", "agent", "mode"} {
			if requiredJSONString(object, context, "assistant message "+field, field) == "" {
				return openCodeMessage{}, fmt.Errorf("opencode %s: known assistant message is missing %s", context, field)
			}
		}
		path, err := requiredJSONObject(object, context, "assistant message path", "path")
		if err != nil || requiredJSONString(path, context, "assistant message path cwd", "cwd") == "" || requiredJSONString(path, context, "assistant message path root", "root") == "" {
			return openCodeMessage{}, fmt.Errorf("opencode %s: known assistant message has incomplete path", context)
		}
		if !hasJSONNumber(object, "cost") {
			return openCodeMessage{}, fmt.Errorf("opencode %s: known assistant message is missing cost", context)
		}
		tokens, err := requiredJSONObject(object, context, "assistant message tokens", "tokens")
		if err != nil || !hasJSONNumber(tokens, "input") || !hasJSONNumber(tokens, "output") || !hasJSONNumber(tokens, "reasoning") {
			return openCodeMessage{}, fmt.Errorf("opencode %s: known assistant message has incomplete tokens", context)
		}
		cache, err := requiredJSONObject(tokens, context, "assistant message tokens cache", "cache")
		if err != nil || !hasJSONNumber(cache, "read") || !hasJSONNumber(cache, "write") {
			return openCodeMessage{}, fmt.Errorf("opencode %s: known assistant message has incomplete token cache", context)
		}
	default:
		return openCodeMessage{}, fmt.Errorf("opencode %s: known message has unsupported role %q", context, message.Role)
	}
	return message, nil
}

func parseOpenCodePart(data []byte, context string) (openCodePart, error) {
	object, err := jsonObject(data, context, "part data")
	if err != nil {
		return openCodePart{}, err
	}
	part := openCodePart{
		ID:        requiredJSONString(object, context, "part id", "id"),
		SessionID: requiredJSONString(object, context, "part sessionID", "sessionID"),
		MessageID: requiredJSONString(object, context, "part messageID", "messageID"),
		Type:      requiredJSONString(object, context, "part type", "type"),
	}
	if part.ID == "" || part.SessionID == "" || part.MessageID == "" || part.Type == "" {
		return openCodePart{}, fmt.Errorf("opencode %s: part data is missing required base fields", context)
	}
	switch part.Type {
	case "text":
		part.Text = requiredJSONString(object, context, "text part text", "text")
		if part.Text == "" {
			return openCodePart{}, fmt.Errorf("opencode %s: known text part is missing text", context)
		}
	case "reasoning":
		part.Text = requiredJSONString(object, context, "reasoning part text", "text")
		time, err := requiredJSONObject(object, context, "reasoning part time", "time")
		if part.Text == "" || err != nil || !hasJSONNumber(time, "start") {
			return openCodePart{}, fmt.Errorf("opencode %s: known reasoning part is missing text or time.start", context)
		}
	case "tool":
		part.CallID = requiredJSONString(object, context, "tool part callID", "callID")
		part.Tool = requiredJSONString(object, context, "tool part tool", "tool")
		state, err := requiredJSONObject(object, context, "tool part state", "state")
		if part.CallID == "" || part.Tool == "" || err != nil || requiredJSONString(state, context, "tool state status", "status") != "completed" {
			return openCodePart{}, fmt.Errorf("opencode %s: known tool part is missing required completed state", context)
		}
		part.Input = state["input"]
		if _, err := requiredJSONObject(state, context, "tool state input", "input"); err != nil || requiredJSONString(state, context, "tool state output", "output") == "" || requiredJSONString(state, context, "tool state title", "title") == "" {
			return openCodePart{}, fmt.Errorf("opencode %s: known completed tool part is missing input, output, or title", context)
		}
		if _, err := requiredJSONObject(state, context, "tool state metadata", "metadata"); err != nil {
			return openCodePart{}, fmt.Errorf("opencode %s: known completed tool part is missing metadata", context)
		}
		time, err := requiredJSONObject(state, context, "tool state time", "time")
		if err != nil || !hasJSONNumber(time, "start") || !hasJSONNumber(time, "end") {
			return openCodePart{}, fmt.Errorf("opencode %s: known completed tool part has incomplete time", context)
		}
	case "file":
		part.MIME = requiredJSONString(object, context, "file part mime", "mime")
		part.URL = requiredJSONString(object, context, "file part url", "url")
		if part.MIME == "" || part.URL == "" {
			return openCodePart{}, fmt.Errorf("opencode %s: known file part is missing required fields", context)
		}
	}
	return part, nil
}

func jsonObject(data []byte, context, subject string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, fmt.Errorf("opencode %s: %s has an invalid JSON shape", context, subject)
	}
	return object, nil
}

func requiredJSONObject(object map[string]json.RawMessage, context, subject, field string) (map[string]json.RawMessage, error) {
	raw := object[field]
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("opencode %s: %s is missing", context, subject)
	}
	return jsonObject(raw, context, subject)
}

func requiredJSONString(object map[string]json.RawMessage, _ string, _ string, field string) string {
	var value string
	if err := json.Unmarshal(object[field], &value); err != nil {
		return ""
	}
	return value
}

func hasJSONNumber(object map[string]json.RawMessage, field string) bool {
	var value json.Number
	return json.Unmarshal(object[field], &value) == nil && value != ""
}

func validateMessageRow(locator sessionio.SourceLocator, row manifestRow, message openCodeMessage) error {
	for _, field := range []struct{ name, value string }{{"id", message.ID}, {"session_id", message.SessionID}} {
		value, err := requiredTextCell(locator, row, field.name)
		if err != nil || value != field.value {
			return fmt.Errorf("opencode %s: message data %s does not match typed row cell", databaseContext(locator), field.name)
		}
	}
	return nil
}

func validatePartRow(locator sessionio.SourceLocator, row manifestRow, part openCodePart) error {
	for _, field := range []struct{ name, value string }{{"id", part.ID}, {"session_id", part.SessionID}, {"message_id", part.MessageID}} {
		value, err := requiredTextCell(locator, row, field.name)
		if err != nil || value != field.value {
			return fmt.Errorf("opencode %s: part data %s does not match typed row cell", databaseContext(locator), field.name)
		}
	}
	return nil
}

type openCodeEventProjection struct {
	nativeKind string
	table      string
	id         string
	sessionID  string
	messageID  string
	relation   sessionio.RelationKind
}

func parseOpenCodeEvent(data []byte, sequence int) (openCodeEventProjection, error) {
	context := fmt.Sprintf("event:%d", sequence)
	object, err := jsonObject(data, context, "event envelope")
	if err != nil {
		return openCodeEventProjection{}, err
	}
	nativeKind := requiredJSONString(object, context, "event type", "type")
	properties, err := requiredJSONObject(object, context, "event properties", "properties")
	if err != nil {
		return openCodeEventProjection{}, err
	}
	switch nativeKind {
	case "message.part.updated":
		part, err := parseOpenCodePart(properties["part"], context)
		if err != nil {
			return openCodeEventProjection{}, err
		}
		return openCodeEventProjection{nativeKind: nativeKind, table: "part", id: part.ID, sessionID: part.SessionID, messageID: part.MessageID, relation: sessionio.RelationKindUpdates}, nil
	case "message.updated":
		message, err := parseOpenCodeMessage(properties["info"], context)
		if err != nil {
			return openCodeEventProjection{}, err
		}
		return openCodeEventProjection{nativeKind: nativeKind, table: "message", id: message.ID, sessionID: message.SessionID, relation: sessionio.RelationKindMaterializes}, nil
	default:
		return openCodeEventProjection{}, fmt.Errorf("opencode %s: unsupported native event type %q", context, nativeKind)
	}
}

func validateEventTarget(sequence int, event openCodeEventProjection, row manifestRow, manifest manifest) error {
	locator, err := manifestRowLocator(manifest, row)
	if err != nil {
		return err
	}
	if event.table == "part" {
		if err := validatePartRow(locator, row, openCodePart{ID: event.id, SessionID: event.sessionID, MessageID: event.messageID}); err != nil {
			return fmt.Errorf("opencode event:%d: %w", sequence, err)
		}
		return nil
	}
	if err := validateMessageRow(locator, row, openCodeMessage{ID: event.id, SessionID: event.sessionID}); err != nil {
		return fmt.Errorf("opencode event:%d: %w", sequence, err)
	}
	return nil
}

func requiredTextCell(locator sessionio.SourceLocator, row manifestRow, name string) (string, error) {
	for _, cell := range row.Cells {
		if len(cell) != 3 {
			continue
		}
		var column, cellType, value string
		if json.Unmarshal(cell[0], &column) == nil && column == name {
			if json.Unmarshal(cell[1], &cellType) != nil || cellType != "TEXT" || json.Unmarshal(cell[2], &value) != nil {
				return "", fmt.Errorf("opencode %s: required SQLite TEXT cell %q is invalid", databaseContext(locator), name)
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("opencode %s: required SQLite TEXT cell %q is missing", databaseContext(locator), name)
}

func requiredIntegerCell(locator sessionio.SourceLocator, row manifestRow, name string) (int64, error) {
	for _, cell := range row.Cells {
		if len(cell) != 3 {
			continue
		}
		var column, cellType string
		if json.Unmarshal(cell[0], &column) == nil && column == name {
			var number json.Number
			if json.Unmarshal(cell[1], &cellType) != nil || cellType != "INTEGER" || json.Unmarshal(cell[2], &number) != nil {
				return 0, fmt.Errorf("opencode %s: required SQLite INTEGER cell %q is invalid", databaseContext(locator), name)
			}
			value, err := number.Int64()
			if err != nil {
				return 0, fmt.Errorf("opencode %s: required SQLite INTEGER cell %q is invalid", databaseContext(locator), name)
			}
			return value, nil
		}
	}
	return 0, fmt.Errorf("opencode %s: required SQLite INTEGER cell %q is missing", databaseContext(locator), name)
}

type eventLine struct {
	Data    []byte
	Framing []byte
	Locator sessionio.SourceLocator
}

func eventLines(path string) (map[int]eventLine, error) {
	data, err := os.ReadFile(fixturePath(path))
	if err != nil {
		return nil, err
	}
	result := map[int]eventLine{}
	var offset int64
	var record uint64
	for _, framed := range bytes.SplitAfter(data, []byte("\n")) {
		if len(framed) == 0 {
			continue
		}
		line := framed
		var framing []byte
		if framed[len(framed)-1] == '\n' {
			line = framed[:len(framed)-1]
			framing = []byte("\n")
		}
		if len(line) == 0 {
			offset += int64(len(framed))
			continue
		}
		recordNumber := record
		lineNumber := record + 1
		byteRange := sessionio.ByteRange{Start: offset, End: offset + int64(len(line))}
		result[int(lineNumber)] = eventLine{
			Data:    append([]byte(nil), line...),
			Framing: framing,
			Locator: sessionio.SourceLocator{
				Kind: sessionio.LocatorKindFile,
				File: &sessionio.FileLocator{
					Root:      fixtureRoot,
					Path:      path,
					Record:    &recordNumber,
					Line:      &lineNumber,
					ByteRange: &byteRange,
				},
			},
		}
		record++
		offset += int64(len(framed))
	}
	return result, nil
}

func databaseLocator(path, table string, keys []sessionio.DatabaseKey) sessionio.SourceLocator {
	return sessionio.SourceLocator{Kind: sessionio.LocatorKindDatabase,
		Database: &sessionio.DatabaseLocator{Path: path, Table: table, Keys: keys}}
}

func keyIdentity(keys []sessionio.DatabaseKey) string {
	parts := make([]string, len(keys))
	for index, key := range keys {
		parts[index] = key.Name + "=" + key.Value
	}
	return strings.Join(parts, "|")
}

func keyValues(keys []sessionio.DatabaseKey) []string {
	values := make([]string, len(keys))
	for index, key := range keys {
		values[index] = key.Value
	}
	return values
}

func relation(id string, kind sessionio.RelationKind, from, to sessionio.NodeRef, origin sessionio.RelationOrigin, observation sessionio.ObservationID, locator sessionio.SourceLocator) sessionio.Relation {
	return sessionio.Relation{ID: sessionio.RelationID(id), Kind: kind, From: from, To: to, Origin: origin,
		Evidence: []sessionio.EvidenceRef{{Observation: observation, Locator: locator}}}
}

func diagnostic(code, message string, locator sessionio.SourceLocator) sessionio.Diagnostic {
	return sessionio.Diagnostic{Code: code, Severity: sessionio.DiagnosticSeverityWarning, Message: message, Locator: &locator}
}

func assertRecordsValidate(t *testing.T, session sessionio.SessionRef, items []sessionio.ReadItem) {
	t.Helper()
	records := recordsForGolden(session, items, sessionio.SessionRef{}, nil)
	if err := sessionio.WriteJSON(io.Discard, sessionio.Producer{Name: "contractstress-check", Version: "1"}, records); err != nil {
		t.Fatalf("contract-stress records do not validate: %v", err)
	}
	for _, item := range items {
		for _, event := range item.Events {
			assertEvidence(t, item.Observation.ID, item.Observation.Locator, event.Evidence)
		}
		for _, relation := range item.Relations {
			assertEvidence(t, item.Observation.ID, item.Observation.Locator, relation.Evidence)
		}
	}
}

func assertEvidence(t *testing.T, id sessionio.ObservationID, locator sessionio.SourceLocator, evidence []sessionio.EvidenceRef) {
	t.Helper()
	for _, reference := range evidence {
		left, _ := json.Marshal(reference.Locator)
		right, _ := json.Marshal(locator)
		if reference.Observation != id || !bytes.Equal(left, right) {
			t.Fatalf("evidence = %#v, want %q %#v", reference, id, locator)
		}
	}
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "contractstress", name)
}

func TestContractStressWriteJSONGolden(t *testing.T) {
	ompRecords, ompRevision := readOMP(t, "omp-append.jsonl")
	ompSession, ompItems := projectOMP(t, ompRecords, ompRevision)
	openSession, openItems, err := projectOpenCode(loadManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := sessionio.WriteJSON(&output, sessionio.Producer{Name: "contractstress", Version: "1"},
		recordsForGolden(ompSession, ompItems, openSession, openItems)); err != nil {
		t.Fatal(err)
	}
	actual := sanitizeJSON(t, output.Bytes())
	golden := fixturePath("contractstress.golden.json")
	if os.Getenv("UPDATE_CONTRACTSTRESS_GOLDEN") == "1" {
		if err := os.WriteFile(golden, actual, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("contract stress JSON golden differs\nactual:\n%s\nexpected:\n%s", actual, expected)
	}
}

func recordsForGolden(first sessionio.SessionRef, firstItems []sessionio.ReadItem, second sessionio.SessionRef, secondItems []sessionio.ReadItem) []sessionio.Record {
	records := make([]sessionio.Record, 0, 2+len(firstItems)+len(secondItems))
	if first.ID != "" {
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindSession, Session: &first})
	}
	for index := range firstItems {
		item := firstItems[index]
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindReadItem, ReadItem: &item})
	}
	if second.ID != "" {
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindSession, Session: &second})
	}
	for index := range secondItems {
		item := secondItems[index]
		records = append(records, sessionio.Record{Kind: sessionio.RecordKindReadItem, ReadItem: &item})
	}
	return records
}

func sanitizeJSON(t *testing.T, encoded []byte) []byte {
	t.Helper()
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	var sanitize func(any) any
	sanitize = func(value any) any {
		switch typed := value.(type) {
		case []any:
			for index := range typed {
				typed[index] = sanitize(typed[index])
			}
		case map[string]any:
			for key, nested := range typed {
				if key == "root" {
					typed[key] = "<fixture-root>"
				} else {
					typed[key] = sanitize(nested)
				}
			}
		}
		return value
	}
	result, err := json.MarshalIndent(sanitize(document), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(result, '\n')
}
