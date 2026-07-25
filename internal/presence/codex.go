package presence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/runtimeprobe"
)

const codexPresenceVersion = "1"

type CodexProviderConfig struct {
	Home      string
	Inspector runtimeprobe.Inspector
}

type codexOpenFileProvider struct {
	home         string
	inspector    runtimeprobe.Inspector
	inspectorErr error
}

// NewCodexOpenFileProvider constructs the provider whose exact signal is a
// validated Codex process holding one exact, adapter-validated rollout path.
func NewCodexOpenFileProvider(config CodexProviderConfig) (Provider, error) {
	home := config.Home
	if home == "" {
		home = os.Getenv("CODEX_HOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("presence: resolve Codex home: %w", err)
			}
			home = filepath.Join(userHome, ".codex")
		}
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("presence: resolve Codex home: %w", err)
	}
	inspector := config.Inspector
	var inspectorErr error
	if inspector == nil {
		inspector, inspectorErr = runtimeprobe.NewInspector()
	}
	return &codexOpenFileProvider{
		home:         filepath.Clean(absoluteHome),
		inspector:    inspector,
		inspectorErr: inspectorErr,
	}, nil
}

func (provider *codexOpenFileProvider) Harness() sessionio.Harness {
	return sessionio.HarnessCodex
}

func (provider *codexOpenFileProvider) Inspect(
	ctx context.Context,
	sessions []sessionio.SessionRef,
) (ProviderResult, error) {
	if provider.inspectorErr != nil || provider.inspector == nil {
		return unavailableInspectorResult(
			provider.inspectorErr,
			codexProviderStatus,
		), nil
	}
	processes, err := provider.inspector.Processes(ctx)
	if err != nil {
		reason := sessionio.PresenceReasonInspectionFailed
		return expectedProviderFailure(
			codexProviderStatus(
				sessionio.PresenceSupportUnavailable,
				&reason,
				err.Error(),
			),
			err,
		)
	}
	codexProcesses := make(map[processKey]runtimeprobe.Process)
	for _, process := range processes {
		if !isCodexProcessName(process.Executable) {
			continue
		}
		current, err := provider.inspector.Process(ctx, process.Identity.PID)
		if err != nil ||
			current.Identity != process.Identity ||
			!isCodexProcessName(current.Executable) {
			continue
		}
		key := processKey{
			pid:       current.Identity.PID,
			startedAt: current.Identity.StartedAt.UTC(),
		}
		codexProcesses[key] = current
	}

	candidates, err := provider.codexFileCandidates(sessions)
	if err != nil {
		return ProviderResult{}, err
	}
	uses, err := provider.inspector.FileUses(ctx, candidatePaths(candidates))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ProviderResult{}, errors.Join(contextErr, err)
		}
		return provider.unavailableCodexFileResult(
			ctx,
			codexProcesses,
			err,
		)
	}

	claimsByProcess := make(map[processKey][]Claim)
	for _, use := range uses {
		key := processKey{
			pid:       use.Process.PID,
			startedAt: use.Process.StartedAt.UTC(),
		}
		if _, found := codexProcesses[key]; !found {
			continue
		}
		for _, session := range candidates[use.Path] {
			claimsByProcess[key] = append(claimsByProcess[key], Claim{
				NativeSessionID: session.NativeID,
				Certainty:       sessionio.PresenceCertaintyExact,
				ExactSessionID:  session.ID,
				Evidence: []sessionio.PresenceEvidence{{
					Kind:      sessionio.PresenceEvidenceOpenSessionFile,
					Certainty: sessionio.PresenceCertaintyExact,
				}},
			})
		}
	}

	result := ProviderResult{
		Status:    codexProviderStatus(sessionio.PresenceSupportSupported, nil, ""),
		Processes: make([]ProcessObservation, 0, len(codexProcesses)),
	}
	for key, process := range codexProcesses {
		current, err := provider.inspector.Process(ctx, key.pid)
		if err != nil || current.Identity != process.Identity {
			continue
		}
		observation := ProcessObservation{
			Process: sessionio.ProcessInstance{
				PID:       key.pid,
				StartedAt: key.startedAt,
				Evidence:  processIdentityEvidence(),
			},
			Claims: claimsByProcess[key],
		}
		if len(observation.Claims) == 0 {
			observation.Reason = sessionio.PresenceReasonNoSessionIdentity
			observation.Evidence = processIdentityEvidence()
		}
		result.Processes = append(result.Processes, observation)
	}
	return result, nil
}

func codexProviderStatus(
	support sessionio.PresenceSupport,
	reason *sessionio.PresenceReason,
	detail string,
) sessionio.PresenceProviderStatus {
	return providerStatus(
		sessionio.HarnessCodex,
		codexPresenceVersion,
		support,
		reason,
		detail,
		"Codex presence uses exact open-rollout ownership",
	)
}

func (provider *codexOpenFileProvider) unavailableCodexFileResult(
	ctx context.Context,
	processes map[processKey]runtimeprobe.Process,
	cause error,
) (ProviderResult, error) {
	reason := sessionio.PresenceReasonProviderUnavailable
	result := ProviderResult{
		Cause:  cause,
		Status: codexProviderStatus(sessionio.PresenceSupportSupported, nil, ""),
	}
	result.Status.Capabilities[0].Support = sessionio.PresenceSupportUnavailable
	result.Status.Capabilities[0].Reason = &reason
	result.Status.Capabilities[0].Detail = cause.Error()
	for key, process := range processes {
		current, err := provider.inspector.Process(ctx, key.pid)
		if err != nil || current.Identity != process.Identity {
			continue
		}
		result.Processes = append(result.Processes, ProcessObservation{
			Process: sessionio.ProcessInstance{
				PID:       key.pid,
				StartedAt: key.startedAt,
				Evidence:  processIdentityEvidence(),
			},
			Reason:   reason,
			Evidence: processIdentityEvidence(),
		})
	}
	return result, nil
}

func (provider *codexOpenFileProvider) codexFileCandidates(
	sessions []sessionio.SessionRef,
) (map[string][]sessionio.SessionRef, error) {
	result := make(map[string][]sessionio.SessionRef)
	for _, session := range sessions {
		if session.Occurrence.Harness != sessionio.HarnessCodex {
			return nil, fmt.Errorf(
				"presence: Codex provider received %q session",
				session.Occurrence.Harness,
			)
		}
		locator := session.Occurrence.Locator
		if locator.Kind != sessionio.LocatorKindFile || locator.File == nil {
			return nil, fmt.Errorf(
				"presence: Codex session %q has no file occurrence locator",
				session.ID,
			)
		}
		if filepath.Clean(locator.File.Root) != provider.home {
			return nil, fmt.Errorf(
				"presence: Codex session %q is outside the configured home",
				session.ID,
			)
		}
		if filepath.IsAbs(locator.File.Path) {
			return nil, fmt.Errorf(
				"presence: Codex session %q has an absolute relative path",
				session.ID,
			)
		}
		path := filepath.Clean(filepath.Join(provider.home, filepath.FromSlash(locator.File.Path)))
		if !pathWithinAnyRoot(path, []string{provider.home}) {
			return nil, fmt.Errorf(
				"presence: Codex session %q escapes the configured home",
				session.ID,
			)
		}
		result[path] = append(result[path], session)
	}
	return result, nil
}

func candidatePaths(candidates map[string][]sessionio.SessionRef) []string {
	result := make([]string, 0, len(candidates))
	for path := range candidates {
		result = append(result, path)
	}
	slices.Sort(result)
	return result
}

func isCodexProcessName(name string) bool {
	return strings.EqualFold(name, "codex") ||
		strings.EqualFold(name, "codex.exe")
}
