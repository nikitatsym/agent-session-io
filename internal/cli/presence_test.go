package cli

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestWritePresenceHumanShowsAmbiguityWithoutRepresentative(t *testing.T) {
	first := testSession(sessionio.HarnessClaude, "session-a")
	first.NativeID = "native-shared"
	first.Title = "first"
	second := testSession(sessionio.HarnessClaude, "session-b")
	second.NativeID = "native-shared"
	second.Title = "second"
	snapshot := testPresenceSnapshot()
	snapshot.Matches = []sessionio.PresenceMatch{{
		Harness:         sessionio.HarnessClaude,
		NativeSessionID: "native-shared",
		Certainty:       sessionio.PresenceCertaintyExact,
		Occurrences: []sessionio.PresenceOccurrence{
			{Session: first, Relation: sessionio.PresenceOccurrenceRelationNativeIdentity},
			{Session: second, Relation: sessionio.PresenceOccurrenceRelationNativeIdentity},
		},
		Selection: sessionio.PresenceSelection{
			Status: sessionio.PresenceSelectionAmbiguous,
		},
		Processes: []sessionio.ProcessInstance{testPresenceProcess()},
		Evidence: []sessionio.PresenceEvidence{{
			Kind:      sessionio.PresenceEvidenceNativeSessionRegistry,
			Certainty: sessionio.PresenceCertaintyExact,
		}},
	}}

	var output bytes.Buffer
	ids := newIDAbbreviator([]string{"session-a", "session-b"})
	if err := writePresenceHuman(&output, snapshot, ids); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"STATE", "HARNESS", "SESSION", "NATIVE ID", "PROCESSES", "EVIDENCE", "TITLE"},
		{"open", "claude", "ambiguous(2)", "native-shared", "42", "native_session_registry", ""},
		{"candidate", "claude", "session-a", "native-shared", "-", "-", "first"},
		{"candidate", "claude", "session-b", "native-shared", "-", "-", "second"},
	}
	if got := tableCells(t, output.String()); !reflect.DeepEqual(got, want) {
		t.Fatalf("presence table = %q, want %q", got, want)
	}
}

func TestWritePresenceHumanShowsUnmatchedReason(t *testing.T) {
	snapshot := testPresenceSnapshot()
	snapshot.UnmatchedProcesses = []sessionio.UnmatchedProcess{{
		Harness:          sessionio.HarnessClaude,
		Process:          testPresenceProcess(),
		ClaimedNativeIDs: []string{"missing"},
		Reason:           sessionio.PresenceReasonUnmatchedNativeIdentity,
		Evidence: []sessionio.PresenceEvidence{{
			Kind:      sessionio.PresenceEvidenceProcessIdentity,
			Certainty: sessionio.PresenceCertaintyExact,
		}},
	}}

	var output bytes.Buffer
	if err := writePresenceHuman(&output, snapshot, newIDAbbreviator(nil)); err != nil {
		t.Fatal(err)
	}
	want := []string{"unmatched process", "claude", "-", "missing", "42", "process_identity", "unmatched_native_identity"}
	if got := tableCells(t, output.String()); len(got) != 2 || !reflect.DeepEqual(got[1], want) {
		t.Fatalf("presence table = %q, want row %q", got, want)
	}
}

func testPresenceSnapshot() sessionio.PresenceSnapshot {
	return sessionio.PresenceSnapshot{
		ObservedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 7, 25, 12, 1, 0, 0, time.UTC),
		Providers: []sessionio.PresenceProviderStatus{{
			Harness: sessionio.HarnessClaude,
			Version: "1-test",
			Support: sessionio.PresenceSupportSupported,
			Capabilities: []sessionio.PresenceCapabilityStatus{{
				Capability: sessionio.PresenceCapabilityExactMatch,
				Support:    sessionio.PresenceSupportSupported,
			}},
		}},
		Matches:            []sessionio.PresenceMatch{},
		UnmatchedProcesses: []sessionio.UnmatchedProcess{},
	}
}

func testPresenceProcess() sessionio.ProcessInstance {
	process := sessionio.ProcessInstance{}
	process.PID = 42
	process.StartedAt = time.Date(2026, 7, 25, 11, 59, 0, 0, time.UTC)
	process.Evidence = processIdentityEvidenceForCLI()
	return process
}

func processIdentityEvidenceForCLI() []sessionio.PresenceEvidence {
	return []sessionio.PresenceEvidence{
		{
			Kind:      sessionio.PresenceEvidenceProcessIdentity,
			Certainty: sessionio.PresenceCertaintyExact,
		},
	}
}
