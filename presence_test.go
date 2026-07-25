package sessionio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPresenceJSONExactShape(t *testing.T) {
	snapshot := fixturePresenceSnapshot()
	var output bytes.Buffer
	if err := WritePresenceJSON(&output, fixtureProducer(), snapshot); err != nil {
		t.Fatalf("WritePresenceJSON() error = %v", err)
	}
	want := "{\"schema\":\"sessionio.presence/v1\",\"producer\":{\"name\":\"sessionio\",\"version\":\"0.0.0-test\"},\"snapshot\":{\"observed_at\":\"2026-07-25T10:00:00Z\",\"expires_at\":\"2026-07-25T10:01:00Z\",\"providers\":[{\"harness\":\"codex\",\"version\":\"0.0.0-test\",\"support\":\"supported\",\"capabilities\":[{\"capability\":\"exact_match\",\"support\":\"supported\"},{\"capability\":\"probable_match\",\"support\":\"supported\"}]}],\"matches\":[{\"harness\":\"codex\",\"native_session_id\":\"native-session-synthetic\",\"certainty\":\"exact\",\"occurrences\":[{\"session\":{\"id\":\"session-synthetic\",\"native_id\":\"native-session-synthetic\",\"discovery_revision\":\"sha256:synthetic-discovery\",\"native\":{},\"occurrence\":{\"id\":\"occurrence-codex-active\",\"source_id\":\"source-codex-active\",\"harness\":\"codex\",\"locator\":{\"kind\":\"file\",\"file\":{\"root\":\"codex-home\",\"path\":\"sessions/2026/07/24/rollout-synthetic.jsonl\"}}}},\"relation\":\"exact_locator\"}],\"selection\":{\"status\":\"resolved\",\"session_id\":\"session-synthetic\"},\"processes\":[{\"pid\":42,\"started_at\":\"2026-07-25T09:59:00Z\",\"evidence\":[{\"kind\":\"process_identity\",\"certainty\":\"exact\"}]}],\"evidence\":[{\"kind\":\"open_session_file\",\"certainty\":\"exact\"}]}],\"unmatched_processes\":[]}}\n"
	if output.String() != want {
		t.Fatalf("WritePresenceJSON() = %s, want %s", output.String(), want)
	}
}

func TestPresenceNDJSONWritesOneSelfContainedLine(t *testing.T) {
	var output bytes.Buffer
	encoder, err := NewPresenceNDJSONEncoder(&output, fixtureProducer())
	if err != nil {
		t.Fatalf("NewPresenceNDJSONEncoder() error = %v", err)
	}
	if err := encoder.Encode(fixturePresenceSnapshot()); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if strings.Count(output.String(), "\n") != 1 || !strings.HasSuffix(output.String(), "\n") {
		t.Fatalf("NDJSON output = %q, want exactly one final newline", output.String())
	}
	var document struct {
		Schema   string           `json:"schema"`
		Producer Producer         `json:"producer"`
		Snapshot PresenceSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("NDJSON line is not self-contained JSON: %v", err)
	}
	if document.Schema != PresenceSchema || document.Snapshot.Matches[0].NativeSessionID != "native-session-synthetic" {
		t.Fatalf("decoded NDJSON document = %#v", document)
	}
}

func TestPresenceAcceptsAmbiguousMatch(t *testing.T) {
	snapshot := fixturePresenceSnapshot()
	snapshot.Matches[0].Occurrences[0].Relation = PresenceOccurrenceRelationNativeIdentity
	copy := snapshot.Matches[0].Occurrences[0].Session
	copy.ID = "session-synthetic-copy"
	copy.Occurrence.ID = "occurrence-codex-archive"
	snapshot.Matches[0].Occurrences = append(snapshot.Matches[0].Occurrences, PresenceOccurrence{
		Session:  copy,
		Relation: PresenceOccurrenceRelationNativeIdentity,
	})
	snapshot.Matches[0].Certainty = PresenceCertaintyProbable
	snapshot.Matches[0].Evidence[0].Certainty = PresenceCertaintyProbable
	snapshot.Matches[0].Selection = PresenceSelection{Status: PresenceSelectionAmbiguous}
	if err := ValidatePresenceSnapshot(snapshot); err != nil {
		t.Fatalf("ValidatePresenceSnapshot() error = %v", err)
	}
}

func TestPresenceRejectsInvalidSnapshotBeforeWriting(t *testing.T) {
	snapshot := fixturePresenceSnapshot()
	snapshot.ExpiresAt = snapshot.ObservedAt
	var output bytes.Buffer
	if err := WritePresenceJSON(&output, fixtureProducer(), snapshot); err == nil {
		t.Fatal("WritePresenceJSON() error = nil, want validation error")
	}
	if output.Len() != 0 {
		t.Fatalf("WritePresenceJSON() wrote %d bytes for an invalid snapshot", output.Len())
	}
}

func TestPresencePreservesCopiedOccurrenceIDs(t *testing.T) {
	snapshot := fixturePresenceSnapshot()
	session := snapshot.Matches[0].Occurrences[0].Session
	var output bytes.Buffer
	if err := WritePresenceJSON(&output, fixtureProducer(), snapshot); err != nil {
		t.Fatalf("WritePresenceJSON() error = %v", err)
	}
	var document struct {
		Snapshot PresenceSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	encoded := document.Snapshot.Matches[0].Occurrences[0].Session
	if encoded.ID != session.ID || encoded.Occurrence.ID != session.Occurrence.ID {
		t.Fatalf("encoded occurrence = %#v, want IDs from %#v", encoded, session)
	}
	if session.ID != snapshot.Matches[0].Occurrences[0].Session.ID ||
		session.Occurrence.ID != snapshot.Matches[0].Occurrences[0].Session.Occurrence.ID {
		t.Fatal("presence encoding mutated the copied session occurrence")
	}
}

func TestPresenceRejectsDuplicateCapabilitiesAndProcesses(t *testing.T) {
	t.Run("capabilities", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Providers[0].Capabilities = append(snapshot.Providers[0].Capabilities, snapshot.Providers[0].Capabilities[0])
		assertPresenceValidationError(t, snapshot, "duplicate value")
	})
	t.Run("processes", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Processes = append(snapshot.Matches[0].Processes, snapshot.Matches[0].Processes[0])
		assertPresenceValidationError(t, snapshot, "duplicate process identity")
	})
	t.Run("unmatched processes", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		unmatched := fixtureUnmatchedProcess()
		snapshot.UnmatchedProcesses = []UnmatchedProcess{unmatched, unmatched}
		assertPresenceValidationError(t, snapshot, "duplicate harness and process identity")
	})
}

func TestPresenceRejectsSelectionAndEvidenceIncoherence(t *testing.T) {
	t.Run("resolved selection outside occurrences", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Selection.SessionID = "invented"
		assertPresenceValidationError(t, snapshot, "must identify one match occurrence")
	})
	t.Run("ambiguous selection with primary", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Selection.Status = PresenceSelectionAmbiguous
		assertPresenceValidationError(t, snapshot, "must be empty for ambiguous")
	})
	t.Run("ambiguous single occurrence", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Selection = PresenceSelection{Status: PresenceSelectionAmbiguous}
		assertPresenceValidationError(t, snapshot, "requires multiple occurrences")
	})
	t.Run("exact match with probable evidence", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Evidence[0].Certainty = PresenceCertaintyProbable
		assertPresenceValidationError(t, snapshot, "exact match requires exact evidence")
	})
	t.Run("match without process", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Processes = nil
		assertPresenceValidationError(t, snapshot, "processes: must not be empty")
	})
}

func TestPresenceRejectsInvalidTimeAndUnmatchedProcess(t *testing.T) {
	t.Run("zero observed time", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.ObservedAt = time.Time{}
		assertPresenceValidationError(t, snapshot, "observed_at")
	})
	t.Run("unmatched without evidence", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.UnmatchedProcesses = []UnmatchedProcess{{
			Harness: HarnessCodex,
			Process: fixturePresenceProcess(),
			Reason:  PresenceReasonNoSessionIdentity,
		}}
		assertPresenceValidationError(t, snapshot, "unmatched_processes[0].evidence: must not be empty")
	})
	t.Run("unmatched without harness", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		unmatched := fixtureUnmatchedProcess()
		unmatched.Harness = ""
		snapshot.UnmatchedProcesses = []UnmatchedProcess{unmatched}
		assertPresenceValidationError(t, snapshot, "harness: must not be empty")
	})
}

func TestPresenceRejectsSupportReasonIncoherence(t *testing.T) {
	t.Run("supported provider with reason", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		reason := PresenceReasonInspectionFailed
		snapshot.Providers[0].Reason = &reason
		assertPresenceValidationError(t, snapshot, "must be empty when support is supported")
	})
	t.Run("unavailable capability without reason", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Providers[0].Capabilities[0].Support = PresenceSupportUnavailable
		assertPresenceValidationError(t, snapshot, "must not be empty when support is not supported")
	})
	t.Run("supported capability under unavailable provider", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		reason := PresenceReasonInspectionFailed
		snapshot.Providers[0].Support = PresenceSupportUnavailable
		snapshot.Providers[0].Reason = &reason
		assertPresenceValidationError(t, snapshot, "cannot be supported when provider is")
	})
}

func TestPresenceRejectsOrphanHarnessAndWeakProcessIdentity(t *testing.T) {
	t.Run("match without provider", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Harness = HarnessClaude
		snapshot.Matches[0].Occurrences[0].Session.Occurrence.Harness = HarnessClaude
		assertPresenceValidationError(t, snapshot, "has no provider status")
	})
	t.Run("process without exact identity evidence", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		snapshot.Matches[0].Processes[0].Evidence[0].Certainty =
			PresenceCertaintyProbable
		assertPresenceValidationError(t, snapshot, "must contain exact process_identity")
	})
	t.Run("duplicate unmatched native identity", func(t *testing.T) {
		snapshot := fixturePresenceSnapshot()
		unmatched := fixtureUnmatchedProcess()
		unmatched.ClaimedNativeIDs = []string{"same", "same"}
		snapshot.UnmatchedProcesses = []UnmatchedProcess{unmatched}
		assertPresenceValidationError(t, snapshot, "duplicate value")
	})
}

func assertPresenceValidationError(t *testing.T, snapshot PresenceSnapshot, expected string) {
	t.Helper()
	err := ValidatePresenceSnapshot(snapshot)
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("ValidatePresenceSnapshot() error = %v, want text %q", err, expected)
	}
}

func fixturePresenceSnapshot() PresenceSnapshot {
	session := fixtureRecords()[1].Session
	return PresenceSnapshot{
		ObservedAt: time.Date(2026, time.July, 25, 10, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, time.July, 25, 10, 1, 0, 0, time.UTC),
		Providers: []PresenceProviderStatus{{
			Harness: HarnessCodex,
			Version: "0.0.0-test",
			Support: PresenceSupportSupported,
			Capabilities: []PresenceCapabilityStatus{
				{Capability: PresenceCapabilityExactMatch, Support: PresenceSupportSupported},
				{Capability: PresenceCapabilityProbableMatch, Support: PresenceSupportSupported},
			},
		}},
		Matches: []PresenceMatch{{
			Harness:         HarnessCodex,
			NativeSessionID: session.NativeID,
			Certainty:       PresenceCertaintyExact,
			Occurrences: []PresenceOccurrence{{
				Session:  *session,
				Relation: PresenceOccurrenceRelationExactLocator,
			}},
			Selection: PresenceSelection{Status: PresenceSelectionResolved, SessionID: session.ID},
			Processes: []ProcessInstance{fixturePresenceProcess()},
			Evidence: []PresenceEvidence{{
				Kind:      PresenceEvidenceOpenSessionFile,
				Certainty: PresenceCertaintyExact,
			}},
		}},
		UnmatchedProcesses: []UnmatchedProcess{},
	}
}

func fixturePresenceProcess() ProcessInstance {
	return ProcessInstance{
		PID:       42,
		StartedAt: time.Date(2026, time.July, 25, 9, 59, 0, 0, time.UTC),
		Evidence: []PresenceEvidence{{
			Kind:      PresenceEvidenceProcessIdentity,
			Certainty: PresenceCertaintyExact,
		}},
	}
}

func fixtureUnmatchedProcess() UnmatchedProcess {
	return UnmatchedProcess{
		Harness: HarnessCodex,
		Process: fixturePresenceProcess(),
		Reason:  PresenceReasonNoSessionIdentity,
		Evidence: []PresenceEvidence{{
			Kind:      PresenceEvidenceProcessIdentity,
			Certainty: PresenceCertaintyExact,
		}},
	}
}
