package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/runtimeprobe"
)

const (
	claudePresenceVersion = "1"
	maxClaudeRegistrySize = 1 << 20
)

type ClaudeProviderConfig struct {
	ConfigDir       string
	ExecutableRoots []string
	Inspector       runtimeprobe.Inspector
}

type claudeProvider struct {
	configDir       string
	executableRoots []string
	inspector       runtimeprobe.Inspector
	inspectorErr    error
}

func NewClaudeProvider(config ClaudeProviderConfig) (Provider, error) {
	configDir := config.ConfigDir
	if configDir == "" {
		configDir = os.Getenv("CLAUDE_CONFIG_DIR")
		if configDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("presence: resolve Claude home: %w", err)
			}
			configDir = filepath.Join(home, ".claude")
		}
	}
	absoluteConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return nil, fmt.Errorf("presence: resolve Claude config directory: %w", err)
	}

	executableRoots := config.ExecutableRoots
	if executableRoots == nil {
		home, homeErr := os.UserHomeDir()
		if homeErr == nil {
			executableRoots = []string{
				filepath.Join(home, ".local", "share", "claude", "versions"),
			}
		}
	}
	resolvedRoots := make([]string, 0, len(executableRoots))
	for _, root := range executableRoots {
		if root == "" {
			return nil, errors.New("presence: Claude executable root must not be empty")
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("presence: resolve Claude executable root: %w", err)
		}
		resolvedRoots = append(resolvedRoots, filepath.Clean(absolute))
	}

	inspector := config.Inspector
	var inspectorErr error
	if inspector == nil {
		inspector, inspectorErr = runtimeprobe.NewInspector()
	}
	return &claudeProvider{
		configDir:       filepath.Clean(absoluteConfigDir),
		executableRoots: resolvedRoots,
		inspector:       inspector,
		inspectorErr:    inspectorErr,
	}, nil
}

func (provider *claudeProvider) Harness() sessionio.Harness {
	return sessionio.HarnessClaude
}

func (provider *claudeProvider) Inspect(
	ctx context.Context,
	_ []sessionio.SessionRef,
) (ProviderResult, error) {
	if provider.inspectorErr != nil || provider.inspector == nil {
		return unavailableInspectorResult(
			provider.inspectorErr,
			claudeProviderStatus,
		), nil
	}

	processes, err := provider.inspector.Processes(ctx)
	if err != nil {
		reason := sessionio.PresenceReasonInspectionFailed
		return expectedProviderFailure(
			claudeProviderStatus(
				sessionio.PresenceSupportUnavailable,
				&reason,
				err.Error(),
			),
			err,
		)
	}
	candidates := make([]runtimeprobe.Process, 0)
	for _, process := range processes {
		candidate, ok := provider.validatedClaudeProcess(ctx, process)
		if ok {
			candidates = append(candidates, candidate)
		}
	}

	result := ProviderResult{
		Status:    claudeProviderStatus(sessionio.PresenceSupportSupported, nil, ""),
		Processes: make([]ProcessObservation, 0, len(candidates)),
	}
	registryDir := filepath.Join(provider.configDir, "sessions")
	if len(candidates) > 0 {
		info, err := os.Lstat(registryDir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			reason := sessionio.PresenceReasonPrerequisiteMissing
			result.Status.Capabilities[0].Support = sessionio.PresenceSupportUnavailable
			result.Status.Capabilities[0].Reason = &reason
			result.Status.Capabilities[0].Detail = "Claude runtime session registry is absent"
		case err != nil:
			return ProviderResult{}, fmt.Errorf("stat Claude runtime session registry: %w", err)
		case info.Mode()&os.ModeSymlink != 0:
			return ProviderResult{}, errors.New("Claude runtime session registry is a symlink")
		case !info.IsDir():
			return ProviderResult{}, errors.New("Claude runtime session registry is not a directory")
		}
	}

	for _, process := range candidates {
		observation, keep, err := provider.inspectClaudeProcess(
			ctx,
			registryDir,
			process,
		)
		if err != nil {
			return ProviderResult{}, err
		}
		if keep {
			result.Processes = append(result.Processes, observation)
		}
	}
	return result, nil
}

func claudeProviderStatus(
	support sessionio.PresenceSupport,
	reason *sessionio.PresenceReason,
	detail string,
) sessionio.PresenceProviderStatus {
	return providerStatus(
		sessionio.HarnessClaude,
		claudePresenceVersion,
		support,
		reason,
		detail,
		"Claude presence uses its exact PID and native-session registry",
	)
}

func (provider *claudeProvider) validatedClaudeProcess(
	ctx context.Context,
	process runtimeprobe.Process,
) (runtimeprobe.Process, bool) {
	name := strings.TrimSuffix(strings.ToLower(process.Executable), ".exe")
	if name != "claude" && !isClaudeVersionName(name) {
		return runtimeprobe.Process{}, false
	}
	current, err := provider.inspector.Process(ctx, process.Identity.PID)
	if err != nil || current.Identity != process.Identity {
		return runtimeprobe.Process{}, false
	}
	currentName := strings.TrimSuffix(strings.ToLower(current.Executable), ".exe")
	if currentName == "claude" {
		return current, true
	}
	if !isClaudeVersionName(currentName) ||
		!pathWithinAnyRoot(current.ExecutablePath, provider.executableRoots) {
		return runtimeprobe.Process{}, false
	}
	return current, true
}

func (provider *claudeProvider) inspectClaudeProcess(
	ctx context.Context,
	registryDir string,
	process runtimeprobe.Process,
) (ProcessObservation, bool, error) {
	instance := sessionio.ProcessInstance{
		PID:       process.Identity.PID,
		StartedAt: process.Identity.StartedAt.UTC(),
		Evidence:  processIdentityEvidence(),
	}
	registryPath := filepath.Join(
		registryDir,
		strconv.FormatUint(process.Identity.PID, 10)+".json",
	)
	registry, found, err := readClaudeRegistry(registryPath)
	if err != nil {
		return ProcessObservation{}, false, err
	}
	if !found {
		return ProcessObservation{
			Process:  instance,
			Reason:   sessionio.PresenceReasonNoSessionIdentity,
			Evidence: processIdentityEvidence(),
		}, true, nil
	}
	if registry.PID != process.Identity.PID {
		return ProcessObservation{}, false, fmt.Errorf(
			"Claude runtime registry %q PID does not match its live process",
			registryPath,
		)
	}
	if registry.SessionID == "" {
		return ProcessObservation{}, false, fmt.Errorf(
			"Claude runtime registry %q has no sessionId",
			registryPath,
		)
	}
	startedAt, err := parseClaudeProcessStart(registry.ProcessStart)
	if err != nil {
		return ProcessObservation{}, false, fmt.Errorf(
			"parse Claude runtime registry %q procStart: %w",
			registryPath,
			err,
		)
	}
	current, err := provider.inspector.Process(ctx, process.Identity.PID)
	if err != nil || current.Identity != process.Identity {
		return ProcessObservation{}, false, nil
	}
	if !current.Identity.StartedAt.Truncate(time.Second).Equal(startedAt) {
		return ProcessObservation{
			Process:  instance,
			Reason:   sessionio.PresenceReasonStaleProcessIdentity,
			Evidence: processIdentityEvidence(),
		}, true, nil
	}
	evidence := []sessionio.PresenceEvidence{{
		Kind:      sessionio.PresenceEvidenceNativeSessionRegistry,
		Certainty: sessionio.PresenceCertaintyExact,
	}}
	return ProcessObservation{
		Process: instance,
		Claims: []Claim{{
			NativeSessionID: registry.SessionID,
			Certainty:       sessionio.PresenceCertaintyExact,
			Evidence:        evidence,
		}},
		Evidence: evidence,
	}, true, nil
}

type claudeRegistry struct {
	PID          uint64 `json:"pid"`
	SessionID    string `json:"sessionId"`
	ProcessStart string `json:"procStart"`
}

func readClaudeRegistry(path string) (claudeRegistry, bool, error) {
	linkInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return claudeRegistry{}, false, nil
	}
	if err != nil {
		return claudeRegistry{}, false, fmt.Errorf("stat Claude runtime registry %q: %w", path, err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return claudeRegistry{}, false, fmt.Errorf("Claude runtime registry %q is a symlink", path)
	}
	if !linkInfo.Mode().IsRegular() {
		return claudeRegistry{}, false, fmt.Errorf("Claude runtime registry %q is not a regular file", path)
	}
	if linkInfo.Size() > maxClaudeRegistrySize {
		return claudeRegistry{}, false, fmt.Errorf(
			"Claude runtime registry %q exceeds %d bytes",
			path,
			maxClaudeRegistrySize,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return claudeRegistry{}, false, fmt.Errorf("open Claude runtime registry %q: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxClaudeRegistrySize+1))
	if err != nil {
		return claudeRegistry{}, false, fmt.Errorf("read Claude runtime registry %q: %w", path, err)
	}
	if len(data) > maxClaudeRegistrySize {
		return claudeRegistry{}, false, fmt.Errorf(
			"Claude runtime registry %q exceeds %d bytes",
			path,
			maxClaudeRegistrySize,
		)
	}
	after, err := file.Stat()
	if err != nil {
		return claudeRegistry{}, false, fmt.Errorf("restat Claude runtime registry %q: %w", path, err)
	}
	current, err := os.Stat(path)
	if err != nil {
		return claudeRegistry{}, false, fmt.Errorf("restat Claude runtime registry path %q: %w", path, err)
	}
	if !os.SameFile(linkInfo, current) ||
		after.Size() != linkInfo.Size() ||
		after.ModTime() != linkInfo.ModTime() ||
		current.Size() != linkInfo.Size() ||
		current.ModTime() != linkInfo.ModTime() {
		return claudeRegistry{}, false, fmt.Errorf("Claude runtime registry %q changed while reading", path)
	}
	var registry claudeRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return claudeRegistry{}, false, fmt.Errorf("parse Claude runtime registry %q: %w", path, err)
	}
	return registry, true, nil
}

func parseClaudeProcessStart(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("value is empty")
	}
	startedAt, err := time.ParseInLocation(
		"Mon Jan 2 15:04:05 2006",
		value,
		time.UTC,
	)
	if err != nil {
		return time.Time{}, err
	}
	return startedAt.UTC(), nil
}

func isClaudeVersionName(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func pathWithinAnyRoot(path string, roots []string) bool {
	if path == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	for _, root := range roots {
		relative, err := filepath.Rel(root, cleanPath)
		if err == nil &&
			relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
			!filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}

func processIdentityEvidence() []sessionio.PresenceEvidence {
	return []sessionio.PresenceEvidence{{
		Kind:      sessionio.PresenceEvidenceProcessIdentity,
		Certainty: sessionio.PresenceCertaintyExact,
	}}
}
