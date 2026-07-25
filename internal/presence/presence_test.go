package presence

import (
	"context"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func TestObserveResolvesOnlyOneExactOccurrence(t *testing.T) {
	sessions := []sessionio.SessionRef{
		fixtureSession(sessionio.HarnessCodex, "session-copy", "occurrence-copy", "native-1"),
		fixtureSession(sessionio.HarnessCodex, "session-live", "occurrence-live", "native-1"),
	}
	provider := fixtureClaimProvider(
		sessionio.HarnessCodex,
		Claim{
			NativeSessionID: "native-1",
			Certainty:       sessionio.PresenceCertaintyExact,
			ExactSessionID:  "session-live",
			Evidence:        exactFileEvidence(),
		},
	)

	snapshot, err := Observe(context.Background(), fixtureRequest(sessions, provider))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(snapshot.Matches))
	}
	match := snapshot.Matches[0]
	if match.Selection.Status != sessionio.PresenceSelectionResolved ||
		match.Selection.SessionID != "session-live" {
		t.Fatalf("unexpected selection: %#v", match.Selection)
	}
	if len(match.Occurrences) != 2 ||
		match.Occurrences[1].Relation != sessionio.PresenceOccurrenceRelationExactLocator {
		t.Fatalf("unexpected occurrences: %#v", match.Occurrences)
	}
}

func TestObserveKeepsCopiedOccurrencesAmbiguous(t *testing.T) {
	sessions := []sessionio.SessionRef{
		fixtureSession(sessionio.HarnessClaude, "session-a", "occurrence-a", "native-1"),
		fixtureSession(sessionio.HarnessClaude, "session-b", "occurrence-b", "native-1"),
	}
	provider := fixtureClaimProvider(
		sessionio.HarnessClaude,
		Claim{
			NativeSessionID: "native-1",
			Certainty:       sessionio.PresenceCertaintyExact,
			Evidence:        registryEvidence(),
		},
	)

	snapshot, err := Observe(context.Background(), fixtureRequest(sessions, provider))
	if err != nil {
		t.Fatal(err)
	}
	match := snapshot.Matches[0]
	if match.Selection.Status != sessionio.PresenceSelectionAmbiguous ||
		match.Selection.SessionID != "" {
		t.Fatalf("unexpected selection: %#v", match.Selection)
	}
	for _, occurrence := range match.Occurrences {
		if occurrence.Relation != sessionio.PresenceOccurrenceRelationNativeIdentity {
			t.Fatalf("unexpected occurrence relation: %#v", occurrence)
		}
	}
}

func TestObserveExactFiltersBeforeGrouping(t *testing.T) {
	sessions := []sessionio.SessionRef{
		fixtureSession(sessionio.HarnessCodex, "session-a", "occurrence-a", "native-1"),
		fixtureSession(sessionio.HarnessCodex, "session-b", "occurrence-b", "native-2"),
	}
	provider := fixtureClaimProvider(
		sessionio.HarnessCodex,
		Claim{
			NativeSessionID: "native-1",
			Certainty:       sessionio.PresenceCertaintyExact,
			Evidence:        exactFileEvidence(),
		},
		Claim{
			NativeSessionID: "native-2",
			Certainty:       sessionio.PresenceCertaintyProbable,
			Evidence: []sessionio.PresenceEvidence{{
				Kind:      sessionio.PresenceEvidenceTerminalBreadcrumb,
				Certainty: sessionio.PresenceCertaintyProbable,
			}},
		},
	)
	request := fixtureRequest(sessions, provider)
	request.Mode = ModeExact

	snapshot, err := Observe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Matches) != 1 ||
		snapshot.Matches[0].NativeSessionID != "native-1" {
		t.Fatalf("unexpected matches: %#v", snapshot.Matches)
	}
	if len(snapshot.UnmatchedProcesses) != 0 {
		t.Fatalf("probable-only process observation survived exact filtering: %#v", snapshot.UnmatchedProcesses)
	}
}

func TestObserveMissingOccurrenceBecomesUnmatched(t *testing.T) {
	provider := fixtureClaimProvider(
		sessionio.HarnessClaude,
		Claim{
			NativeSessionID: "missing-native",
			Certainty:       sessionio.PresenceCertaintyExact,
			Evidence:        registryEvidence(),
		},
	)

	snapshot, err := Observe(context.Background(), fixtureRequest(nil, provider))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Matches) != 0 || len(snapshot.UnmatchedProcesses) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	unmatched := snapshot.UnmatchedProcesses[0]
	if unmatched.Reason != sessionio.PresenceReasonUnmatchedNativeIdentity ||
		len(unmatched.ClaimedNativeIDs) != 1 ||
		unmatched.ClaimedNativeIDs[0] != "missing-native" {
		t.Fatalf("unexpected unmatched process: %#v", unmatched)
	}
}

func TestObserveOneProcessCanJoinMultipleGroups(t *testing.T) {
	sessions := []sessionio.SessionRef{
		fixtureSession(sessionio.HarnessClaude, "session-parent", "occurrence-parent", "parent"),
		fixtureSession(sessionio.HarnessClaude, "session-agent", "occurrence-agent", "agent"),
	}
	provider := fixtureClaimProvider(
		sessionio.HarnessClaude,
		Claim{
			NativeSessionID: "parent",
			Certainty:       sessionio.PresenceCertaintyExact,
			Evidence:        registryEvidence(),
		},
		Claim{
			NativeSessionID: "agent",
			Certainty:       sessionio.PresenceCertaintyExact,
			Evidence:        registryEvidence(),
		},
	)

	snapshot, err := Observe(context.Background(), fixtureRequest(sessions, provider))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(snapshot.Matches))
	}
	for _, match := range snapshot.Matches {
		if len(match.Processes) != 1 || match.Processes[0].PID != 42 {
			t.Fatalf("unexpected match process: %#v", match)
		}
	}
}

func TestObserveRejectsExactLocatorOutsideNativeCandidates(t *testing.T) {
	session := fixtureSession(
		sessionio.HarnessCodex,
		"session-a",
		"occurrence-a",
		"native-1",
	)
	provider := fixtureClaimProvider(
		sessionio.HarnessCodex,
		Claim{
			NativeSessionID: "native-1",
			Certainty:       sessionio.PresenceCertaintyExact,
			ExactSessionID:  "invented",
			Evidence:        exactFileEvidence(),
		},
	)

	_, err := Observe(
		context.Background(),
		fixtureRequest([]sessionio.SessionRef{session}, provider),
	)
	if err == nil {
		t.Fatal("Observe() accepted an invented exact occurrence")
	}
}

type fakeProvider struct {
	harness sessionio.Harness
	result  ProviderResult
	err     error
}

func fixtureClaimProvider(
	harness sessionio.Harness,
	claims ...Claim,
) fakeProvider {
	return fakeProvider{
		harness: harness,
		result: ProviderResult{
			Status: fixtureProviderStatus(harness),
			Processes: []ProcessObservation{{
				Process: fixtureProcess(42),
				Claims:  claims,
			}},
		},
	}
}

func (provider fakeProvider) Harness() sessionio.Harness {
	return provider.harness
}

func (provider fakeProvider) Inspect(
	context.Context,
	[]sessionio.SessionRef,
) (ProviderResult, error) {
	return provider.result, provider.err
}

func fixtureRequest(sessions []sessionio.SessionRef, provider Provider) Request {
	return Request{
		ObservedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		Mode:       ModeAll,
		Sessions:   sessions,
		Providers:  []Provider{provider},
	}
}

func fixtureProviderStatus(harness sessionio.Harness) sessionio.PresenceProviderStatus {
	return sessionio.PresenceProviderStatus{
		Harness: harness,
		Version: "1-test",
		Support: sessionio.PresenceSupportSupported,
		Capabilities: []sessionio.PresenceCapabilityStatus{{
			Capability: sessionio.PresenceCapabilityExactMatch,
			Support:    sessionio.PresenceSupportSupported,
		}},
	}
}

func fixtureSession(
	harness sessionio.Harness,
	id sessionio.SessionID,
	occurrenceID sessionio.OccurrenceID,
	nativeID string,
) sessionio.SessionRef {
	return sessionio.SessionRef{
		ID:                id,
		NativeID:          nativeID,
		DiscoveryRevision: "revision",
		Occurrence: sessionio.SourceOccurrence{
			ID:       occurrenceID,
			SourceID: "source",
			Harness:  harness,
			Locator: sessionio.SourceLocator{
				Kind: sessionio.LocatorKindFile,
				File: &sessionio.FileLocator{Root: "/root", Path: "session.jsonl"},
			},
		},
	}
}

func fixtureProcess(pid uint64) sessionio.ProcessInstance {
	return sessionio.ProcessInstance{
		PID:       pid,
		StartedAt: time.Date(2026, 7, 25, 11, 59, 0, 0, time.UTC),
		Evidence: []sessionio.PresenceEvidence{{
			Kind:      sessionio.PresenceEvidenceProcessIdentity,
			Certainty: sessionio.PresenceCertaintyExact,
		}},
	}
}

func exactFileEvidence() []sessionio.PresenceEvidence {
	return []sessionio.PresenceEvidence{{
		Kind:      sessionio.PresenceEvidenceOpenSessionFile,
		Certainty: sessionio.PresenceCertaintyExact,
	}}
}

func registryEvidence() []sessionio.PresenceEvidence {
	return []sessionio.PresenceEvidence{{
		Kind:      sessionio.PresenceEvidenceNativeSessionRegistry,
		Certainty: sessionio.PresenceCertaintyExact,
	}}
}
