package presence

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/runtimeprobe"
)

const DefaultTTL = time.Minute

type Mode string

const (
	ModeAll   Mode = "all"
	ModeExact Mode = "exact"
)

type Claim struct {
	NativeSessionID string
	Certainty       sessionio.PresenceCertainty
	ExactSessionID  sessionio.SessionID
	Evidence        []sessionio.PresenceEvidence
}

type ProcessObservation struct {
	Process  sessionio.ProcessInstance
	Claims   []Claim
	Reason   sessionio.PresenceReason
	Evidence []sessionio.PresenceEvidence
}

type ProviderResult struct {
	Status    sessionio.PresenceProviderStatus
	Processes []ProcessObservation
	Cause     error
}

type providerStatusFactory func(
	sessionio.PresenceSupport,
	*sessionio.PresenceReason,
	string,
) sessionio.PresenceProviderStatus

func expectedProviderFailure(
	status sessionio.PresenceProviderStatus,
	cause error,
) (ProviderResult, error) {
	return ProviderResult{Status: status, Cause: cause}, nil
}

func unavailableInspectorResult(
	cause error,
	status providerStatusFactory,
) ProviderResult {
	reason := sessionio.PresenceReasonProviderUnsupported
	detail := "runtime process inspection is unavailable on this platform"
	if cause != nil && !errors.Is(cause, runtimeprobe.ErrUnsupported) {
		reason = sessionio.PresenceReasonProviderUnavailable
		detail = cause.Error()
	}
	return ProviderResult{
		Status: status(
			sessionio.PresenceSupportUnsupported,
			&reason,
			detail,
		),
		Cause: cause,
	}
}

func providerStatus(
	harness sessionio.Harness,
	version string,
	support sessionio.PresenceSupport,
	reason *sessionio.PresenceReason,
	detail string,
	probableDetail string,
) sessionio.PresenceProviderStatus {
	probableReason := sessionio.PresenceReasonProviderUnsupported
	return sessionio.PresenceProviderStatus{
		Harness: harness,
		Version: version,
		Support: support,
		Reason:  reason,
		Detail:  detail,
		Capabilities: []sessionio.PresenceCapabilityStatus{
			{
				Capability: sessionio.PresenceCapabilityExactMatch,
				Support:    support,
				Reason:     reason,
				Detail:     detail,
			},
			{
				Capability: sessionio.PresenceCapabilityProbableMatch,
				Support:    sessionio.PresenceSupportUnsupported,
				Reason:     &probableReason,
				Detail:     probableDetail,
			},
		},
	}
}

type Provider interface {
	Harness() sessionio.Harness
	Inspect(context.Context, []sessionio.SessionRef) (ProviderResult, error)
}

type Request struct {
	ObservedAt time.Time
	TTL        time.Duration
	Mode       Mode
	Sessions   []sessionio.SessionRef
	Providers  []Provider
}

func Observe(ctx context.Context, request Request) (sessionio.PresenceSnapshot, error) {
	if request.ObservedAt.IsZero() {
		return sessionio.PresenceSnapshot{}, errors.New("presence: observed time must not be zero")
	}
	if request.TTL == 0 {
		request.TTL = DefaultTTL
	}
	if request.TTL < 0 {
		return sessionio.PresenceSnapshot{}, errors.New("presence: TTL must be positive")
	}
	if request.Mode != ModeAll && request.Mode != ModeExact {
		return sessionio.PresenceSnapshot{}, fmt.Errorf("presence: unsupported mode %q", request.Mode)
	}
	if len(request.Providers) == 0 {
		return sessionio.PresenceSnapshot{}, errors.New("presence: providers must not be empty")
	}

	sessionsByHarness := make(map[sessionio.Harness][]sessionio.SessionRef)
	for _, session := range request.Sessions {
		harness := session.Occurrence.Harness
		sessionsByHarness[harness] = append(sessionsByHarness[harness], session)
	}

	snapshot := sessionio.PresenceSnapshot{
		ObservedAt:         request.ObservedAt.UTC(),
		ExpiresAt:          request.ObservedAt.Add(request.TTL).UTC(),
		Providers:          make([]sessionio.PresenceProviderStatus, 0, len(request.Providers)),
		Matches:            []sessionio.PresenceMatch{},
		UnmatchedProcesses: []sessionio.UnmatchedProcess{},
	}
	seenProviders := make(map[sessionio.Harness]struct{}, len(request.Providers))
	groups := make(map[matchKey]*matchGroup)

	for _, provider := range request.Providers {
		if provider == nil {
			return sessionio.PresenceSnapshot{}, errors.New("presence: provider must not be nil")
		}
		harness := provider.Harness()
		if harness == "" {
			return sessionio.PresenceSnapshot{}, errors.New("presence: provider harness must not be empty")
		}
		if _, exists := seenProviders[harness]; exists {
			return sessionio.PresenceSnapshot{}, fmt.Errorf("presence: duplicate provider for harness %q", harness)
		}
		seenProviders[harness] = struct{}{}

		result, err := provider.Inspect(ctx, slices.Clone(sessionsByHarness[harness]))
		if err != nil {
			return sessionio.PresenceSnapshot{}, fmt.Errorf("inspect %s runtime presence: %w", harness, err)
		}
		if result.Status.Harness != harness {
			return sessionio.PresenceSnapshot{}, fmt.Errorf(
				"presence: provider %q returned status for harness %q",
				harness,
				result.Status.Harness,
			)
		}
		snapshot.Providers = append(snapshot.Providers, result.Status)
		for index, process := range result.Processes {
			if err := addProcessObservation(
				request.Mode,
				harness,
				sessionsByHarness[harness],
				process,
				groups,
				&snapshot.UnmatchedProcesses,
			); err != nil {
				return sessionio.PresenceSnapshot{}, fmt.Errorf(
					"presence: invalid %s process observation %d: %w",
					harness,
					index,
					err,
				)
			}
		}
	}

	for _, group := range groups {
		snapshot.Matches = append(snapshot.Matches, group.match())
	}
	sortSnapshot(&snapshot)
	if err := sessionio.ValidatePresenceSnapshot(snapshot); err != nil {
		return sessionio.PresenceSnapshot{}, fmt.Errorf("presence: assemble snapshot: %w", err)
	}
	return snapshot, nil
}

type matchKey struct {
	harness         sessionio.Harness
	nativeSessionID string
}

type matchGroup struct {
	key             matchKey
	sessions        []sessionio.SessionRef
	exactSessionIDs map[sessionio.SessionID]struct{}
	processes       map[processKey]sessionio.ProcessInstance
	evidence        []sessionio.PresenceEvidence
	exact           bool
}

type processKey struct {
	pid       uint64
	startedAt time.Time
}

func addProcessObservation(
	mode Mode,
	harness sessionio.Harness,
	sessions []sessionio.SessionRef,
	observation ProcessObservation,
	groups map[matchKey]*matchGroup,
	unmatched *[]sessionio.UnmatchedProcess,
) error {
	if observation.Process.PID == 0 || observation.Process.StartedAt.IsZero() {
		return errors.New("process identity must include PID and start time")
	}
	claims := make([]Claim, 0, len(observation.Claims))
	for _, claim := range observation.Claims {
		if claim.NativeSessionID == "" {
			return errors.New("claim native session ID must not be empty")
		}
		if claim.Certainty != sessionio.PresenceCertaintyExact &&
			claim.Certainty != sessionio.PresenceCertaintyProbable {
			return fmt.Errorf("claim has unsupported certainty %q", claim.Certainty)
		}
		if claim.ExactSessionID != "" && claim.Certainty != sessionio.PresenceCertaintyExact {
			return errors.New("exact session ID requires exact certainty")
		}
		if mode == ModeExact && claim.Certainty != sessionio.PresenceCertaintyExact {
			continue
		}
		claims = append(claims, claim)
	}
	if mode == ModeExact && len(observation.Claims) > 0 && len(claims) == 0 {
		return nil
	}

	matched := false
	claimedNativeIDs := make([]string, 0, len(claims))
	for _, claim := range claims {
		claimedNativeIDs = append(claimedNativeIDs, claim.NativeSessionID)
		candidates := sessionsWithNativeID(sessions, claim.NativeSessionID)
		if len(candidates) == 0 {
			continue
		}
		matched = true
		key := matchKey{harness: harness, nativeSessionID: claim.NativeSessionID}
		group := groups[key]
		if group == nil {
			group = &matchGroup{
				key:             key,
				sessions:        candidates,
				exactSessionIDs: make(map[sessionio.SessionID]struct{}),
				processes:       make(map[processKey]sessionio.ProcessInstance),
			}
			groups[key] = group
		}
		if claim.ExactSessionID != "" {
			if !hasSessionID(candidates, claim.ExactSessionID) {
				return fmt.Errorf(
					"exact session ID %q is not a candidate for native session %q",
					claim.ExactSessionID,
					claim.NativeSessionID,
				)
			}
			group.exactSessionIDs[claim.ExactSessionID] = struct{}{}
		}
		process := cloneProcess(observation.Process)
		process.Evidence = append(process.Evidence, claim.Evidence...)
		process.Evidence = normalizedEvidence(process.Evidence)
		identity := processKey{pid: process.PID, startedAt: process.StartedAt.UTC()}
		process.StartedAt = identity.startedAt
		if existing, exists := group.processes[identity]; exists {
			existing.Evidence = normalizedEvidence(append(existing.Evidence, process.Evidence...))
			group.processes[identity] = existing
		} else {
			group.processes[identity] = process
		}
		group.evidence = append(group.evidence, claim.Evidence...)
		if claim.Certainty == sessionio.PresenceCertaintyExact {
			group.exact = true
		}
	}

	if matched {
		return nil
	}
	reason := observation.Reason
	if reason == "" {
		if len(claimedNativeIDs) > 0 {
			reason = sessionio.PresenceReasonUnmatchedNativeIdentity
		} else {
			reason = sessionio.PresenceReasonNoSessionIdentity
		}
	}
	evidence := normalizedEvidence(append(
		slices.Clone(observation.Evidence),
		observation.Process.Evidence...,
	))
	if len(evidence) == 0 {
		return errors.New("unmatched process evidence must not be empty")
	}
	process := cloneProcess(observation.Process)
	process.StartedAt = process.StartedAt.UTC()
	process.Evidence = normalizedEvidence(process.Evidence)
	*unmatched = append(*unmatched, sessionio.UnmatchedProcess{
		Harness:          harness,
		Process:          process,
		ClaimedNativeIDs: uniqueStrings(claimedNativeIDs),
		Reason:           reason,
		Evidence:         evidence,
	})
	return nil
}

func (group *matchGroup) match() sessionio.PresenceMatch {
	occurrences := make([]sessionio.PresenceOccurrence, 0, len(group.sessions))
	for _, session := range group.sessions {
		relation := sessionio.PresenceOccurrenceRelationNativeIdentity
		if _, exact := group.exactSessionIDs[session.ID]; exact {
			relation = sessionio.PresenceOccurrenceRelationExactLocator
		}
		occurrences = append(occurrences, sessionio.PresenceOccurrence{
			Session:  session,
			Relation: relation,
		})
	}

	selection := sessionio.PresenceSelection{Status: sessionio.PresenceSelectionAmbiguous}
	switch {
	case len(group.exactSessionIDs) == 1:
		selection.Status = sessionio.PresenceSelectionResolved
		for sessionID := range group.exactSessionIDs {
			selection.SessionID = sessionID
		}
	case len(occurrences) == 1:
		selection.Status = sessionio.PresenceSelectionResolved
		selection.SessionID = occurrences[0].Session.ID
	}

	processes := make([]sessionio.ProcessInstance, 0, len(group.processes))
	for _, process := range group.processes {
		processes = append(processes, process)
	}
	certainty := sessionio.PresenceCertaintyProbable
	if group.exact {
		certainty = sessionio.PresenceCertaintyExact
	}
	return sessionio.PresenceMatch{
		Harness:         group.key.harness,
		NativeSessionID: group.key.nativeSessionID,
		Certainty:       certainty,
		Occurrences:     occurrences,
		Selection:       selection,
		Processes:       processes,
		Evidence:        normalizedEvidence(group.evidence),
	}
}

func sessionsWithNativeID(
	sessions []sessionio.SessionRef,
	nativeID string,
) []sessionio.SessionRef {
	result := make([]sessionio.SessionRef, 0)
	for _, session := range sessions {
		if session.NativeID == nativeID {
			result = append(result, session)
		}
	}
	return result
}

func hasSessionID(sessions []sessionio.SessionRef, id sessionio.SessionID) bool {
	for _, session := range sessions {
		if session.ID == id {
			return true
		}
	}
	return false
}

func cloneProcess(process sessionio.ProcessInstance) sessionio.ProcessInstance {
	process.Evidence = slices.Clone(process.Evidence)
	return process
}
