package sessionio

import (
	"fmt"
	"time"
)

// ValidatePresenceSnapshot validates a runtime-presence snapshot before use or encoding.
func ValidatePresenceSnapshot(snapshot PresenceSnapshot) error {
	if snapshot.ObservedAt.IsZero() {
		return invalid("snapshot.observed_at", "must not be zero")
	}
	if snapshot.ExpiresAt.IsZero() {
		return invalid("snapshot.expires_at", "must not be zero")
	}
	if !snapshot.ExpiresAt.After(snapshot.ObservedAt) {
		return invalid("snapshot.expires_at", "must be after observed_at")
	}
	if len(snapshot.Providers) == 0 {
		return invalid("snapshot.providers", "must not be empty")
	}
	seenProviders := make(map[Harness]struct{}, len(snapshot.Providers))
	for index, provider := range snapshot.Providers {
		if err := validatePresenceProvider(fmt.Sprintf("snapshot.providers[%d]", index), provider); err != nil {
			return err
		}
		if _, exists := seenProviders[provider.Harness]; exists {
			return invalid(fmt.Sprintf("snapshot.providers[%d].harness", index), "duplicate value %q", provider.Harness)
		}
		seenProviders[provider.Harness] = struct{}{}
	}
	seenMatches := make(map[presenceMatchIdentity]struct{}, len(snapshot.Matches))
	for index, match := range snapshot.Matches {
		if err := validatePresenceMatch(fmt.Sprintf("snapshot.matches[%d]", index), match); err != nil {
			return err
		}
		if _, exists := seenProviders[match.Harness]; !exists {
			return invalid(
				fmt.Sprintf("snapshot.matches[%d].harness", index),
				"has no provider status",
			)
		}
		identity := presenceMatchIdentity{harness: match.Harness, nativeSessionID: match.NativeSessionID}
		if _, exists := seenMatches[identity]; exists {
			return invalid(fmt.Sprintf("snapshot.matches[%d]", index), "duplicate harness and native_session_id")
		}
		seenMatches[identity] = struct{}{}
	}
	seenUnmatched := make(map[unmatchedProcessIdentity]struct{}, len(snapshot.UnmatchedProcesses))
	for index, unmatched := range snapshot.UnmatchedProcesses {
		if err := validateUnmatchedProcess(fmt.Sprintf("snapshot.unmatched_processes[%d]", index), unmatched); err != nil {
			return err
		}
		if _, exists := seenProviders[unmatched.Harness]; !exists {
			return invalid(
				fmt.Sprintf("snapshot.unmatched_processes[%d].harness", index),
				"has no provider status",
			)
		}
		identity := unmatchedProcessIdentity{
			harness:   unmatched.Harness,
			pid:       unmatched.Process.PID,
			startedAt: unmatched.Process.StartedAt,
		}
		if _, exists := seenUnmatched[identity]; exists {
			return invalid(fmt.Sprintf("snapshot.unmatched_processes[%d]", index), "duplicate harness and process identity")
		}
		seenUnmatched[identity] = struct{}{}
	}
	return nil
}

type presenceMatchIdentity struct {
	harness         Harness
	nativeSessionID string
}

func validatePresenceProvider(path string, provider PresenceProviderStatus) error {
	if provider.Harness == "" {
		return invalid(path+".harness", "must not be empty")
	}
	if provider.Version == "" {
		return invalid(path+".version", "must not be empty")
	}
	if err := validatePresenceSupport(path+".support", provider.Support); err != nil {
		return err
	}
	if err := validatePresenceReasonPointer(path+".reason", provider.Reason); err != nil {
		return err
	}
	if err := validatePresenceSupportReason(path, provider.Support, provider.Reason); err != nil {
		return err
	}
	if len(provider.Capabilities) == 0 {
		return invalid(path+".capabilities", "must not be empty")
	}
	seen := make(map[PresenceCapability]struct{}, len(provider.Capabilities))
	for index, capability := range provider.Capabilities {
		itemPath := fmt.Sprintf("%s.capabilities[%d]", path, index)
		if !validPresenceCapability(capability.Capability) {
			return invalid(itemPath+".capability", "unsupported value %q", capability.Capability)
		}
		if _, exists := seen[capability.Capability]; exists {
			return invalid(itemPath+".capability", "duplicate value %q", capability.Capability)
		}
		seen[capability.Capability] = struct{}{}
		if err := validatePresenceSupport(itemPath+".support", capability.Support); err != nil {
			return err
		}
		if err := validatePresenceReasonPointer(itemPath+".reason", capability.Reason); err != nil {
			return err
		}
		if err := validatePresenceSupportReason(itemPath, capability.Support, capability.Reason); err != nil {
			return err
		}
		if provider.Support != PresenceSupportSupported &&
			capability.Support == PresenceSupportSupported {
			return invalid(
				itemPath+".support",
				"cannot be supported when provider is %q",
				provider.Support,
			)
		}
	}
	return nil
}

func validatePresenceMatch(path string, match PresenceMatch) error {
	if match.Harness == "" {
		return invalid(path+".harness", "must not be empty")
	}
	if match.NativeSessionID == "" {
		return invalid(path+".native_session_id", "must not be empty")
	}
	if !validPresenceCertainty(match.Certainty) {
		return invalid(path+".certainty", "unsupported value %q", match.Certainty)
	}
	if len(match.Occurrences) == 0 {
		return invalid(path+".occurrences", "must not be empty")
	}
	seenOccurrences := make(map[SessionID]struct{}, len(match.Occurrences))
	selected := false
	selectedExactLocator := false
	exactLocators := 0
	for index, occurrence := range match.Occurrences {
		itemPath := fmt.Sprintf("%s.occurrences[%d]", path, index)
		if err := validateSessionRef(itemPath+".session", occurrence.Session); err != nil {
			return err
		}
		if occurrence.Session.Occurrence.Harness != match.Harness {
			return invalid(itemPath+".session.occurrence.harness", "must equal match harness")
		}
		if occurrence.Session.NativeID != match.NativeSessionID {
			return invalid(itemPath+".session.native_id", "must equal match native_session_id")
		}
		if !validPresenceOccurrenceRelation(occurrence.Relation) {
			return invalid(itemPath+".relation", "unsupported value %q", occurrence.Relation)
		}
		if occurrence.Relation == PresenceOccurrenceRelationExactLocator {
			exactLocators++
		}
		if _, exists := seenOccurrences[occurrence.Session.ID]; exists {
			return invalid(itemPath+".session.id", "duplicate occurrence %q", occurrence.Session.ID)
		}
		seenOccurrences[occurrence.Session.ID] = struct{}{}
		if occurrence.Session.ID == match.Selection.SessionID {
			selected = true
			selectedExactLocator = occurrence.Relation == PresenceOccurrenceRelationExactLocator
		}
	}
	if err := validatePresenceSelection(
		path+".selection",
		match.Selection,
		len(match.Occurrences),
		exactLocators,
		selected,
		selectedExactLocator,
	); err != nil {
		return err
	}
	if len(match.Processes) == 0 {
		return invalid(path+".processes", "must not be empty")
	}
	seenProcesses := make(map[processIdentity]struct{}, len(match.Processes))
	for index, process := range match.Processes {
		itemPath := fmt.Sprintf("%s.processes[%d]", path, index)
		if err := validateProcessInstance(itemPath, process); err != nil {
			return err
		}
		identity := processIdentity{pid: process.PID, startedAt: process.StartedAt}
		if _, exists := seenProcesses[identity]; exists {
			return invalid(itemPath, "duplicate process identity")
		}
		seenProcesses[identity] = struct{}{}
	}
	if err := validatePresenceEvidence(path+".evidence", match.Evidence, true); err != nil {
		return err
	}
	if match.Certainty == PresenceCertaintyExact && !hasExactPresenceEvidence(match.Evidence) {
		return invalid(path+".evidence", "exact match requires exact evidence")
	}
	return nil
}

func validatePresenceSelection(
	path string,
	selection PresenceSelection,
	occurrences int,
	exactLocators int,
	selected bool,
	selectedExactLocator bool,
) error {
	switch selection.Status {
	case PresenceSelectionResolved:
		if selection.SessionID == "" || !selected {
			return invalid(path+".session_id", "must identify one match occurrence for resolved selection")
		}
		if occurrences > 1 && (exactLocators != 1 || !selectedExactLocator) {
			return invalid(path, "multiple occurrences resolve only through one exact locator")
		}
	case PresenceSelectionAmbiguous:
		if selection.SessionID != "" {
			return invalid(path+".session_id", "must be empty for ambiguous selection")
		}
		if occurrences < 2 {
			return invalid(path+".status", "ambiguous selection requires multiple occurrences")
		}
		if exactLocators == 1 {
			return invalid(path, "one exact locator must resolve its occurrence")
		}
	default:
		return invalid(path+".status", "unsupported value %q", selection.Status)
	}
	return nil
}

func validateUnmatchedProcess(path string, unmatched UnmatchedProcess) error {
	if unmatched.Harness == "" {
		return invalid(path+".harness", "must not be empty")
	}
	if err := validateProcessInstance(path+".process", unmatched.Process); err != nil {
		return err
	}
	if !validPresenceReason(unmatched.Reason) {
		return invalid(path+".reason", "unsupported value %q", unmatched.Reason)
	}
	seenNativeIDs := make(map[string]struct{}, len(unmatched.ClaimedNativeIDs))
	for index, nativeID := range unmatched.ClaimedNativeIDs {
		if nativeID == "" {
			return invalid(fmt.Sprintf("%s.claimed_native_ids[%d]", path, index), "must not be empty")
		}
		if _, exists := seenNativeIDs[nativeID]; exists {
			return invalid(
				fmt.Sprintf("%s.claimed_native_ids[%d]", path, index),
				"duplicate value %q",
				nativeID,
			)
		}
		seenNativeIDs[nativeID] = struct{}{}
	}
	return validatePresenceEvidence(path+".evidence", unmatched.Evidence, true)
}

type processIdentity struct {
	pid       uint64
	startedAt time.Time
}

type unmatchedProcessIdentity struct {
	harness   Harness
	pid       uint64
	startedAt time.Time
}

func validateProcessInstance(path string, process ProcessInstance) error {
	if process.PID == 0 {
		return invalid(path+".pid", "must be positive")
	}
	if process.StartedAt.IsZero() {
		return invalid(path+".started_at", "must not be zero")
	}
	if err := validatePresenceEvidence(path+".evidence", process.Evidence, true); err != nil {
		return err
	}
	for _, evidence := range process.Evidence {
		if evidence.Kind == PresenceEvidenceProcessIdentity &&
			evidence.Certainty == PresenceCertaintyExact {
			return nil
		}
	}
	return invalid(
		path+".evidence",
		"must contain exact process_identity evidence",
	)
}

func validatePresenceEvidence(path string, evidence []PresenceEvidence, required bool) error {
	if required && len(evidence) == 0 {
		return invalid(path, "must not be empty")
	}
	for index, item := range evidence {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		if !validPresenceEvidenceKind(item.Kind) {
			return invalid(itemPath+".kind", "unsupported value %q", item.Kind)
		}
		if !validPresenceCertainty(item.Certainty) {
			return invalid(itemPath+".certainty", "unsupported value %q", item.Certainty)
		}
	}
	return nil
}

func validatePresenceSupport(path string, support PresenceSupport) error {
	if !validPresenceSupport(support) {
		return invalid(path, "unsupported value %q", support)
	}
	return nil
}

func validatePresenceReasonPointer(path string, reason *PresenceReason) error {
	if reason != nil && !validPresenceReason(*reason) {
		return invalid(path, "unsupported value %q", *reason)
	}
	return nil
}

func validatePresenceSupportReason(
	path string,
	support PresenceSupport,
	reason *PresenceReason,
) error {
	if support == PresenceSupportSupported && reason != nil {
		return invalid(path+".reason", "must be empty when support is supported")
	}
	if support != PresenceSupportSupported && reason == nil {
		return invalid(path+".reason", "must not be empty when support is not supported")
	}
	return nil
}

func hasExactPresenceEvidence(evidence []PresenceEvidence) bool {
	for _, item := range evidence {
		if item.Certainty == PresenceCertaintyExact {
			return true
		}
	}
	return false
}

func validPresenceCapability(value PresenceCapability) bool {
	return value == PresenceCapabilityExactMatch || value == PresenceCapabilityProbableMatch
}

func validPresenceSupport(value PresenceSupport) bool {
	return value == PresenceSupportSupported || value == PresenceSupportUnavailable || value == PresenceSupportUnsupported
}

func validPresenceReason(value PresenceReason) bool {
	switch value {
	case PresenceReasonAccessDenied,
		PresenceReasonAuthenticationRequired,
		PresenceReasonCrossEnvironment,
		PresenceReasonInspectionFailed,
		PresenceReasonNoSessionIdentity,
		PresenceReasonPrerequisiteMissing,
		PresenceReasonProcessExited,
		PresenceReasonProviderUnavailable,
		PresenceReasonProviderUnsupported,
		PresenceReasonStaleProcessIdentity,
		PresenceReasonUnmatchedNativeIdentity:
		return true
	default:
		return false
	}
}

func validPresenceCertainty(value PresenceCertainty) bool {
	return value == PresenceCertaintyExact || value == PresenceCertaintyProbable
}

func validPresenceEvidenceKind(value PresenceEvidenceKind) bool {
	switch value {
	case PresenceEvidenceProcessIdentity,
		PresenceEvidenceNativeSessionRegistry,
		PresenceEvidenceOpenSessionFile,
		PresenceEvidenceTerminalIdentity,
		PresenceEvidenceTerminalBreadcrumb,
		PresenceEvidenceLoopbackListener,
		PresenceEvidenceHealthEndpoint,
		PresenceEvidenceSessionStatus:
		return true
	default:
		return false
	}
}

func validPresenceOccurrenceRelation(value PresenceOccurrenceRelation) bool {
	return value == PresenceOccurrenceRelationExactLocator || value == PresenceOccurrenceRelationNativeIdentity
}
