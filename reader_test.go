package sessionio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestE2EReaderMachineOutputGolden(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		var output bytes.Buffer
		if err := WriteJSON(&output, fixtureProducer(), fixtureRecords()); err != nil {
			t.Fatalf("WriteJSON() error = %v", err)
		}
		assertGolden(t, "testdata/reader-v1.json", output.Bytes())
	})

	t.Run("ndjson", func(t *testing.T) {
		var output bytes.Buffer
		encoder, err := NewNDJSONEncoder(&output, fixtureProducer())
		if err != nil {
			t.Fatalf("NewNDJSONEncoder() error = %v", err)
		}
		for _, record := range fixtureRecords() {
			if err := encoder.Encode(record); err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
		}
		assertGolden(t, "testdata/reader-v1.ndjson", output.Bytes())
	})
}

func TestMachineOutputRejectsInvalidTaggedUnions(t *testing.T) {
	t.Run("record kind with wrong payload", func(t *testing.T) {
		records := fixtureRecords()
		records[0].Kind = RecordKindSession
		assertWriteJSONError(t, records, "requires session variant")
	})

	t.Run("event kind with wrong payload", func(t *testing.T) {
		records := fixtureRecords()
		records[2].ReadItem.Events[0].Kind = EventKindMessage
		assertWriteJSONError(t, records, "requires message variant")
	})

	t.Run("content kind with wrong payload", func(t *testing.T) {
		records := fixtureRecords()
		event := &records[2].ReadItem.Events[0]
		event.Kind = EventKindMessage
		event.Unknown = nil
		event.Message = &MessageEvent{
			Role: MessageRoleAssistant,
			Content: []ContentBlock{{
				ID:           "content-synthetic",
				Kind:         ContentKindText,
				Availability: ContentAvailabilityAvailable,
				Media:        &MediaContent{MediaType: "image/png"},
			}},
		}
		assertWriteJSONError(t, records, "requires text variant")
	})
}

func TestNDJSONRejectsInvalidRecordBeforeWriting(t *testing.T) {
	var output bytes.Buffer
	encoder, err := NewNDJSONEncoder(&output, fixtureProducer())
	if err != nil {
		t.Fatalf("NewNDJSONEncoder() error = %v", err)
	}
	record := fixtureRecords()[0]
	record.Kind = RecordKindSession
	if err := encoder.Encode(record); err == nil {
		t.Fatal("Encode() error = nil, want validation error")
	}
	if output.Len() != 0 {
		t.Fatalf("Encode() wrote %d bytes for an invalid record", output.Len())
	}
}

func TestJSONRejectsAllRecordsBeforeWriting(t *testing.T) {
	var output bytes.Buffer
	records := fixtureRecords()
	records[3].Diagnostic.Severity = "synthetic-invalid"
	if err := WriteJSON(&output, fixtureProducer(), records); err == nil {
		t.Fatal("WriteJSON() error = nil, want validation error")
	}
	if output.Len() != 0 {
		t.Fatalf("WriteJSON() wrote %d bytes before validation completed", output.Len())
	}
}

func TestMachineOutputRejectsMissingEvidence(t *testing.T) {
	records := fixtureRecords()
	records[2].ReadItem.Events[0].Evidence = nil
	assertWriteJSONError(t, records, "evidence: must not be empty")
}

func TestMachineOutputAcceptsActiveLeafRelation(t *testing.T) {
	records := fixtureRecords()
	records[2].ReadItem.Relations = append(records[2].ReadItem.Relations, Relation{
		ID:     "relation-active-leaf",
		Kind:   RelationKindActiveLeaf,
		From:   NodeRef{Kind: NodeKindSession, ID: string(records[2].ReadItem.Session.ID)},
		To:     NodeRef{Kind: NodeKindObservation, ID: string(records[2].ReadItem.Observation.ID)},
		Origin: RelationOriginDeterministic,
		Evidence: []EvidenceRef{{
			Observation: records[2].ReadItem.Observation.ID,
			Locator:     records[2].ReadItem.Observation.Locator,
		}},
	})
	if err := WriteJSON(io.Discard, fixtureProducer(), records); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
}

func TestMachineOutputRejectsInvalidLocatorsAndCapture(t *testing.T) {
	t.Run("two locator variants", func(t *testing.T) {
		records := fixtureRecords()
		records[2].ReadItem.Observation.Locator.Database = &DatabaseLocator{
			Path:  "sessions.db",
			Table: "events",
		}
		assertWriteJSONError(t, records, "exactly one locator variant")
	})

	t.Run("structured snapshot with framing", func(t *testing.T) {
		records := fixtureRecords()
		records[2].ReadItem.Observation.Representation.Capture = CaptureKindStructuredSnapshot
		assertWriteJSONError(t, records, "only valid for byte-exact or decoded-stream capture")
	})

	t.Run("reversed byte range", func(t *testing.T) {
		records := fixtureRecords()
		records[2].ReadItem.Observation.Locator.File.ByteRange = &ByteRange{
			Start: 10,
			End:   9,
		}
		assertWriteJSONError(t, records, "end must not precede start")
	})
}

func TestRegistryRejectsDuplicateHarness(t *testing.T) {
	first := testAdapter{descriptor: fixtureDescriptor(HarnessCodex)}
	second := testAdapter{descriptor: fixtureDescriptor(HarnessCodex)}
	if _, err := NewRegistry(first, second); err == nil ||
		!strings.Contains(err.Error(), "already registered") {
		t.Fatalf("NewRegistry() error = %v, want duplicate error", err)
	}
}

func TestRegistryValidatesAndOrdersDescriptors(t *testing.T) {
	registry, err := NewRegistry(
		testAdapter{descriptor: fixtureDescriptor(HarnessOpenCode)},
		testAdapter{descriptor: fixtureDescriptor(HarnessClaude)},
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	descriptors := registry.Descriptors()
	if len(descriptors) != 2 {
		t.Fatalf("Descriptors() length = %d, want 2", len(descriptors))
	}
	if descriptors[0].Harness != HarnessClaude || descriptors[1].Harness != HarnessOpenCode {
		t.Fatalf("Descriptors() harnesses = %q, %q", descriptors[0].Harness, descriptors[1].Harness)
	}

	descriptors[0].Capabilities[0].Support = SupportUnavailable
	fresh := registry.Descriptors()
	if fresh[0].Capabilities[0].Support != SupportFull {
		t.Fatal("Descriptors() returned mutable registry state")
	}
}

func TestRegistryRejectsInvalidCapabilityMatrix(t *testing.T) {
	descriptor := fixtureDescriptor(HarnessCodex)
	descriptor.Capabilities = append(descriptor.Capabilities, descriptor.Capabilities[0])
	if _, err := NewRegistry(testAdapter{descriptor: descriptor}); err == nil ||
		!strings.Contains(err.Error(), "duplicate value") {
		t.Fatalf("NewRegistry() error = %v, want capability matrix error", err)
	}
}

func TestRegistryRejectsIncompleteDescriptor(t *testing.T) {
	tests := []struct {
		name       string
		descriptor AdapterDescriptor
		expected   string
	}{
		{
			name: "empty harness",
			descriptor: AdapterDescriptor{
				Version:      "0.0.0-test",
				Capabilities: fixtureDescriptor(HarnessCodex).Capabilities,
			},
			expected: "adapter.harness",
		},
		{
			name: "empty version",
			descriptor: AdapterDescriptor{
				Harness:      HarnessCodex,
				Capabilities: fixtureDescriptor(HarnessCodex).Capabilities,
			},
			expected: "adapter.version",
		},
		{
			name: "empty capability matrix",
			descriptor: AdapterDescriptor{
				Harness: HarnessCodex,
				Version: "0.0.0-test",
			},
			expected: "adapter.capabilities",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(testAdapter{descriptor: test.descriptor}); err == nil ||
				!strings.Contains(err.Error(), test.expected) {
				t.Fatalf("NewRegistry() error = %v, want text %q", err, test.expected)
			}
		})
	}
}

func TestStreamRejectsNextAfterClose(t *testing.T) {
	stream, err := NewStream(
		func(context.Context) (string, error) {
			return "unexpected", nil
		},
		func() error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Next() error = %v, want ErrStreamClosed", err)
	}
}

func TestStreamChecksCanceledContextBeforeCallback(t *testing.T) {
	called := false
	stream, err := NewStream(
		func(context.Context) (string, error) {
			called = true
			return "unexpected", nil
		},
		func() error {
			return nil
		},
	)
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("Next() invoked callback for canceled context")
	}
}

func TestStreamCompletionAndCloseAreStable(t *testing.T) {
	closeFailure := errors.New("synthetic close failure")
	nextCalls := 0
	closeCalls := 0
	stream, err := NewStream(
		func(context.Context) (string, error) {
			nextCalls++
			return "", io.EOF
		},
		func() error {
			closeCalls++
			return closeFailure
		},
	)
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}

	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("first Next() error = %v, want io.EOF", err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next() error = %v, want io.EOF", err)
	}
	if nextCalls != 1 {
		t.Fatalf("next callback calls = %d, want 1", nextCalls)
	}
	if err := stream.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("first Close() error = %v, want close failure", err)
	}
	if err := stream.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("second Close() error = %v, want close failure", err)
	}
	if closeCalls != 1 {
		t.Fatalf("close callback calls = %d, want 1", closeCalls)
	}
}

func TestReaderErrorIncludesContextAndUnwraps(t *testing.T) {
	cause := errors.New("synthetic failure")
	locator := fixtureRecordLocator()
	readerError := &ReaderError{
		Operation:      "read",
		Harness:        HarnessCodex,
		AdapterVersion: "1.2.3",
		SessionID:      "session-synthetic",
		Locator:        &locator,
		Err:            cause,
	}
	for _, expected := range []string{
		"operation=read",
		"harness=codex",
		"adapter_version=1.2.3",
		"session_id=session-synthetic",
		"locator=file:codex-home/sessions/2026/07/24/rollout-synthetic.jsonl",
		"record=1",
		"line=1",
		"bytes=0-56",
		"cause=synthetic failure",
	} {
		if !strings.Contains(readerError.Error(), expected) {
			t.Fatalf("Error() = %q, want %q", readerError.Error(), expected)
		}
	}
	if !errors.Is(readerError, cause) {
		t.Fatal("ReaderError does not unwrap its cause")
	}
}

func assertGolden(t *testing.T, path string, actual []byte) {
	t.Helper()
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("machine output differs from %s\nactual:\n%s\nexpected:\n%s", path, actual, expected)
	}
	if len(actual) == 0 || actual[len(actual)-1] != '\n' {
		t.Fatalf("machine output for %s lacks final newline", path)
	}
}

func assertWriteJSONError(t *testing.T, records []Record, expected string) {
	t.Helper()
	err := WriteJSON(io.Discard, fixtureProducer(), records)
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("WriteJSON() error = %v, want text %q", err, expected)
	}
}

func fixtureProducer() Producer {
	return Producer{
		Name:    "sessionio",
		Version: "0.0.0-test",
	}
}

func fixtureDescriptor(harness Harness) AdapterDescriptor {
	return AdapterDescriptor{
		Harness: harness,
		Version: "0.0.0-test",
		Capabilities: []CapabilityStatus{{
			Capability: CapabilityDiscovery,
			Support:    SupportFull,
		}},
	}
}

func fixtureRecords() []Record {
	sourceLocator := SourceLocator{
		Kind: LocatorKindFile,
		File: &FileLocator{
			Root: "codex-home",
			Path: "sessions",
		},
	}
	sessionLocator := SourceLocator{
		Kind: LocatorKindFile,
		File: &FileLocator{
			Root: "codex-home",
			Path: "sessions/2026/07/24/rollout-synthetic.jsonl",
		},
	}
	recordLocator := fixtureRecordLocator()
	revision := Revision{
		Kind:  RevisionKindFileSnapshot,
		Value: "sha256:synthetic",
	}
	session := SessionRef{
		ID:                "session-synthetic",
		NativeID:          "native-session-synthetic",
		DiscoveryRevision: DiscoveryRevision("sha256:synthetic-discovery"),
		Occurrence: SourceOccurrence{
			ID:       "occurrence-codex-active",
			SourceID: "source-codex-active",
			Harness:  HarnessCodex,
			Locator:  sessionLocator,
		},
	}
	evidence := []EvidenceRef{{
		Observation: "observation-future-record",
		Locator:     recordLocator,
	}}

	source := Source{
		ID:      "source-codex-active",
		Harness: HarnessCodex,
		Kind:    SourceKindCanonical,
		Status:  SourceStatusAvailable,
		Locator: sourceLocator,
		Capabilities: []CapabilityStatus{{
			Capability: CapabilityDiscovery,
			Support:    SupportFull,
		}},
	}
	readItem := ReadItem{
		Session: session,
		Observation: NativeObservation{
			ID:         "observation-future-record",
			NativeKind: "future_record",
			Locator:    recordLocator,
			Revision:   revision,
			Representation: NativeRepresentation{
				Capture:   CaptureKindByteExact,
				MediaType: "application/json",
				Data:      []byte(`{"type":"future_record","payload":{"value":"synthetic"}}`),
				Framing:   []byte("\n"),
			},
		},
		Events: []Event{{
			ID:       "event-future-record",
			Kind:     EventKindUnknown,
			Evidence: evidence,
			Unknown: &UnknownEvent{
				NativeType: "future_record",
			},
		}},
		Relations: []Relation{{
			ID:       "relation-observation-event",
			Kind:     RelationKindContains,
			From:     NodeRef{Kind: NodeKindObservation, ID: "observation-future-record"},
			To:       NodeRef{Kind: NodeKindEvent, ID: "event-future-record"},
			Origin:   RelationOriginDeterministic,
			Evidence: evidence,
		}},
	}
	diagnostic := Diagnostic{
		Code:     "synthetic_warning",
		Severity: DiagnosticSeverityWarning,
		Message:  "synthetic fixture warning",
		Locator:  &recordLocator,
	}

	return []Record{
		{Kind: RecordKindSource, Source: &source},
		{Kind: RecordKindSession, Session: &session},
		{Kind: RecordKindReadItem, ReadItem: &readItem},
		{Kind: RecordKindDiagnostic, Diagnostic: &diagnostic},
	}
}

func fixtureRecordLocator() SourceLocator {
	record := uint64(1)
	line := uint64(1)
	return SourceLocator{
		Kind: LocatorKindFile,
		File: &FileLocator{
			Root:   "codex-home",
			Path:   "sessions/2026/07/24/rollout-synthetic.jsonl",
			Record: &record,
			Line:   &line,
			ByteRange: &ByteRange{
				Start: 0,
				End:   56,
			},
		},
	}
}

type testAdapter struct {
	descriptor AdapterDescriptor
}

func (adapter testAdapter) Descriptor() AdapterDescriptor {
	return adapter.descriptor
}

func (testAdapter) Sources(context.Context) (Stream[Source], error) {
	return nil, errors.New("not implemented")
}

func (testAdapter) Sessions(context.Context, SessionRequest) (Stream[SessionRef], error) {
	return nil, errors.New("not implemented")
}

func (testAdapter) Read(context.Context, SessionRef) (Stream[ReadItem], error) {
	return nil, errors.New("not implemented")
}
