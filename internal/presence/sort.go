package presence

import (
	"cmp"
	"slices"

	sessionio "github.com/nikitatsym/agent-session-io"
)

func sortSnapshot(snapshot *sessionio.PresenceSnapshot) {
	slices.SortFunc(snapshot.Providers, func(left, right sessionio.PresenceProviderStatus) int {
		return cmp.Compare(left.Harness, right.Harness)
	})
	for index := range snapshot.Providers {
		slices.SortFunc(
			snapshot.Providers[index].Capabilities,
			func(left, right sessionio.PresenceCapabilityStatus) int {
				return cmp.Compare(left.Capability, right.Capability)
			},
		)
	}
	for index := range snapshot.Matches {
		match := &snapshot.Matches[index]
		slices.SortFunc(match.Occurrences, func(left, right sessionio.PresenceOccurrence) int {
			return cmp.Compare(left.Session.ID, right.Session.ID)
		})
		slices.SortFunc(match.Processes, compareProcess)
		match.Evidence = normalizedEvidence(match.Evidence)
	}
	slices.SortFunc(snapshot.Matches, func(left, right sessionio.PresenceMatch) int {
		if order := cmp.Compare(left.Harness, right.Harness); order != 0 {
			return order
		}
		return cmp.Compare(left.NativeSessionID, right.NativeSessionID)
	})
	for index := range snapshot.UnmatchedProcesses {
		unmatched := &snapshot.UnmatchedProcesses[index]
		unmatched.ClaimedNativeIDs = uniqueStrings(unmatched.ClaimedNativeIDs)
		unmatched.Process.Evidence = normalizedEvidence(unmatched.Process.Evidence)
		unmatched.Evidence = normalizedEvidence(unmatched.Evidence)
	}
	slices.SortFunc(
		snapshot.UnmatchedProcesses,
		func(left, right sessionio.UnmatchedProcess) int {
			if order := cmp.Compare(left.Harness, right.Harness); order != 0 {
				return order
			}
			return compareProcess(left.Process, right.Process)
		},
	)
}

func compareProcess(left, right sessionio.ProcessInstance) int {
	if order := cmp.Compare(left.PID, right.PID); order != 0 {
		return order
	}
	return left.StartedAt.Compare(right.StartedAt)
}

func normalizedEvidence(evidence []sessionio.PresenceEvidence) []sessionio.PresenceEvidence {
	result := slices.Clone(evidence)
	slices.SortFunc(result, func(left, right sessionio.PresenceEvidence) int {
		if order := cmp.Compare(left.Kind, right.Kind); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Certainty, right.Certainty); order != 0 {
			return order
		}
		return cmp.Compare(left.Detail, right.Detail)
	})
	return slices.Compact(result)
}

func uniqueStrings(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}
