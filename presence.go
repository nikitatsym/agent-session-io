package sessionio

import "time"

// PresenceCapability identifies a runtime-presence signal a provider can inspect.
type PresenceCapability string

const (
	PresenceCapabilityExactMatch    PresenceCapability = "exact_match"
	PresenceCapabilityProbableMatch PresenceCapability = "probable_match"
)

// PresenceSupport describes whether a runtime-presence signal is available.
type PresenceSupport string

const (
	PresenceSupportSupported   PresenceSupport = "supported"
	PresenceSupportUnavailable PresenceSupport = "unavailable"
	PresenceSupportUnsupported PresenceSupport = "unsupported"
)

// PresenceReason explains a non-match or unavailable runtime-presence signal.
type PresenceReason string

const (
	PresenceReasonAccessDenied            PresenceReason = "access_denied"
	PresenceReasonAuthenticationRequired  PresenceReason = "authentication_required"
	PresenceReasonCrossEnvironment        PresenceReason = "cross_environment"
	PresenceReasonInspectionFailed        PresenceReason = "inspection_failed"
	PresenceReasonNoSessionIdentity       PresenceReason = "no_session_identity"
	PresenceReasonPrerequisiteMissing     PresenceReason = "prerequisite_missing"
	PresenceReasonProcessExited           PresenceReason = "process_exited"
	PresenceReasonProviderUnavailable     PresenceReason = "provider_unavailable"
	PresenceReasonProviderUnsupported     PresenceReason = "provider_unsupported"
	PresenceReasonStaleProcessIdentity    PresenceReason = "stale_process_identity"
	PresenceReasonUnmatchedNativeIdentity PresenceReason = "unmatched_native_identity"
)

// PresenceCertainty describes the strength of a runtime-to-session match.
type PresenceCertainty string

const (
	PresenceCertaintyExact    PresenceCertainty = "exact"
	PresenceCertaintyProbable PresenceCertainty = "probable"
)

// PresenceEvidenceKind identifies a non-secret runtime-presence observation.
type PresenceEvidenceKind string

const (
	PresenceEvidenceProcessIdentity       PresenceEvidenceKind = "process_identity"
	PresenceEvidenceNativeSessionRegistry PresenceEvidenceKind = "native_session_registry"
	PresenceEvidenceOpenSessionFile       PresenceEvidenceKind = "open_session_file"
	PresenceEvidenceTerminalIdentity      PresenceEvidenceKind = "terminal_identity"
	PresenceEvidenceTerminalBreadcrumb    PresenceEvidenceKind = "terminal_breadcrumb"
	PresenceEvidenceLoopbackListener      PresenceEvidenceKind = "loopback_listener"
	PresenceEvidenceHealthEndpoint        PresenceEvidenceKind = "health_endpoint"
	PresenceEvidenceSessionStatus         PresenceEvidenceKind = "session_status"
)

// PresenceEvidence is a typed, non-secret observation supporting a presence result.
type PresenceEvidence struct {
	Kind      PresenceEvidenceKind `json:"kind"`
	Certainty PresenceCertainty    `json:"certainty"`
	Detail    string               `json:"detail,omitempty"`
}

// PresenceCapabilityStatus reports one provider capability for one harness.
type PresenceCapabilityStatus struct {
	Capability PresenceCapability `json:"capability"`
	Support    PresenceSupport    `json:"support"`
	Reason     *PresenceReason    `json:"reason,omitempty"`
	Detail     string             `json:"detail,omitempty"`
}

// PresenceProviderStatus reports a versioned runtime-presence provider for one harness.
type PresenceProviderStatus struct {
	Harness      Harness                    `json:"harness"`
	Version      string                     `json:"version"`
	Support      PresenceSupport            `json:"support"`
	Reason       *PresenceReason            `json:"reason,omitempty"`
	Detail       string                     `json:"detail,omitempty"`
	Capabilities []PresenceCapabilityStatus `json:"capabilities"`
}

// ProcessInstance is a live process identity sampled with its start time.
type ProcessInstance struct {
	PID       uint64             `json:"pid"`
	StartedAt time.Time          `json:"started_at"`
	Evidence  []PresenceEvidence `json:"evidence"`
}

// PresenceOccurrenceRelation describes how a persisted occurrence matches a native session.
type PresenceOccurrenceRelation string

const (
	PresenceOccurrenceRelationExactLocator   PresenceOccurrenceRelation = "exact_locator"
	PresenceOccurrenceRelationNativeIdentity PresenceOccurrenceRelation = "native_identity"
)

// PresenceOccurrence attaches one persisted session occurrence to a native session match.
type PresenceOccurrence struct {
	Session  SessionRef                 `json:"session"`
	Relation PresenceOccurrenceRelation `json:"relation"`
}

// PresenceSelectionStatus describes whether one persisted occurrence was selected.
type PresenceSelectionStatus string

const (
	PresenceSelectionResolved  PresenceSelectionStatus = "resolved"
	PresenceSelectionAmbiguous PresenceSelectionStatus = "ambiguous"
)

// PresenceSelection records the selected persisted occurrence, if resolution was possible.
type PresenceSelection struct {
	Status    PresenceSelectionStatus `json:"status"`
	SessionID SessionID               `json:"session_id,omitempty"`
}

// PresenceMatch groups runtime observations by harness and native session ID.
type PresenceMatch struct {
	Harness         Harness              `json:"harness"`
	NativeSessionID string               `json:"native_session_id"`
	Certainty       PresenceCertainty    `json:"certainty"`
	Occurrences     []PresenceOccurrence `json:"occurrences"`
	Selection       PresenceSelection    `json:"selection"`
	Processes       []ProcessInstance    `json:"processes"`
	Evidence        []PresenceEvidence   `json:"evidence"`
}

// UnmatchedProcess is a live harness process that was not assigned to a persisted session.
type UnmatchedProcess struct {
	Harness          Harness            `json:"harness"`
	Process          ProcessInstance    `json:"process"`
	ClaimedNativeIDs []string           `json:"claimed_native_ids,omitempty"`
	Reason           PresenceReason     `json:"reason"`
	Evidence         []PresenceEvidence `json:"evidence"`
}

// PresenceSnapshot is one ephemeral, time-bounded runtime-presence observation.
type PresenceSnapshot struct {
	ObservedAt         time.Time                `json:"observed_at"`
	ExpiresAt          time.Time                `json:"expires_at"`
	Providers          []PresenceProviderStatus `json:"providers"`
	Matches            []PresenceMatch          `json:"matches"`
	UnmatchedProcesses []UnmatchedProcess       `json:"unmatched_processes"`
}
