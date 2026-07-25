package presence

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/runtimeprobe"
)

func TestClaudeProviderMatchesLiveRegistryByPIDAndStart(t *testing.T) {
	root := t.TempDir()
	registryDir := makeClaudeRegistryDir(t, root)
	start := fixtureProcessStart()
	writeClaudeRegistryFixture(t, registryDir, 42, "native-1", start)
	result := inspectNamedClaudeProcess(t, root, start)
	if result.Status.Support != sessionio.PresenceSupportSupported ||
		len(result.Processes) != 1 ||
		len(result.Processes[0].Claims) != 1 ||
		result.Processes[0].Claims[0].NativeSessionID != "native-1" {
		t.Fatalf("unexpected provider result: %#v", result)
	}
}

func TestClaudeProviderRejectsStaleRegistryIdentity(t *testing.T) {
	root := t.TempDir()
	registryDir := makeClaudeRegistryDir(t, root)
	liveStart := fixtureProcessStart()
	writeClaudeRegistryFixture(
		t,
		registryDir,
		42,
		"native-1",
		liveStart.Add(-time.Hour),
	)
	result := inspectNamedClaudeProcess(t, root, liveStart)
	if len(result.Processes) != 1 ||
		len(result.Processes[0].Claims) != 0 ||
		result.Processes[0].Reason != sessionio.PresenceReasonStaleProcessIdentity {
		t.Fatalf("unexpected provider result: %#v", result)
	}
}

func TestClaudeProviderRequiresTrustedPathForVersionNamedProcess(t *testing.T) {
	root := t.TempDir()
	trusted := filepath.Join(root, "versions")
	start := fixtureProcessStart()
	inspector := &fakeInspector{processes: []runtimeprobe.Process{
		fixtureRuntimeProcess(
			42, start, "2.1.220",
			filepath.Join(root, "untrusted", "2.1.220"),
		),
		fixtureRuntimeProcess(
			43, start, "2.1.220",
			filepath.Join(trusted, "2.1.220"),
		),
	}}
	result := inspectClaudeProvider(t, ClaudeProviderConfig{
		ConfigDir:       root,
		ExecutableRoots: []string{trusted},
		Inspector:       inspector,
	})
	if len(result.Processes) != 1 || result.Processes[0].Process.PID != 43 {
		t.Fatalf("unexpected processes: %#v", result.Processes)
	}
}

func TestClaudeProviderMissingRegistryIsTypedUnavailable(t *testing.T) {
	root := t.TempDir()
	start := fixtureProcessStart()
	result := inspectNamedClaudeProcess(t, root, start)
	if result.Status.Support != sessionio.PresenceSupportSupported ||
		result.Status.Capabilities[0].Support != sessionio.PresenceSupportUnavailable ||
		result.Status.Capabilities[0].Reason == nil ||
		*result.Status.Capabilities[0].Reason != sessionio.PresenceReasonPrerequisiteMissing ||
		len(result.Processes) != 1 ||
		result.Processes[0].Reason != sessionio.PresenceReasonNoSessionIdentity {
		t.Fatalf("unexpected provider result: %#v", result)
	}
}

func inspectNamedClaudeProcess(
	t *testing.T,
	root string,
	start time.Time,
) ProviderResult {
	t.Helper()
	inspector := &fakeInspector{processes: []runtimeprobe.Process{
		fixtureRuntimeProcess(42, start, "claude", ""),
	}}
	return inspectClaudeProvider(t, ClaudeProviderConfig{
		ConfigDir: root, Inspector: inspector,
	})
}

func inspectClaudeProvider(
	t *testing.T,
	config ClaudeProviderConfig,
) ProviderResult {
	t.Helper()
	provider, err := NewClaudeProvider(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Inspect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func makeClaudeRegistryDir(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "sessions")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureProcessStart() time.Time {
	return time.Date(2026, 7, 25, 10, 11, 12, 0, time.UTC)
}

func fixtureRuntimeProcess(
	pid uint64,
	start time.Time,
	executable string,
	path string,
) runtimeprobe.Process {
	return runtimeprobe.Process{
		Identity:       runtimeprobe.ProcessIdentity{PID: pid, StartedAt: start},
		Executable:     executable,
		ExecutablePath: path,
	}
}

func writeClaudeRegistryFixture(
	t *testing.T,
	dir string,
	pid uint64,
	sessionID string,
	start time.Time,
) {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	data := []byte(fmt.Sprintf(
		"{\"pid\":%d,\"sessionId\":%q,\"procStart\":%q}\n",
		pid,
		sessionID,
		start.UTC().Format("Mon Jan 2 15:04:05 2006"),
	))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeInspector struct {
	processes   []runtimeprobe.Process
	fileUses    []runtimeprobe.FileUse
	err         error
	fileUsesErr error
}

func (inspector *fakeInspector) Processes(
	context.Context,
) ([]runtimeprobe.Process, error) {
	return append([]runtimeprobe.Process(nil), inspector.processes...), inspector.err
}

func (inspector *fakeInspector) Process(
	_ context.Context,
	pid uint64,
) (runtimeprobe.Process, error) {
	for _, process := range inspector.processes {
		if process.Identity.PID == pid {
			return process, nil
		}
	}
	return runtimeprobe.Process{}, runtimeprobe.ErrProcessNotFound
}

func (inspector *fakeInspector) FileUses(
	context.Context,
	[]string,
) ([]runtimeprobe.FileUse, error) {
	return append([]runtimeprobe.FileUse(nil), inspector.fileUses...), inspector.fileUsesErr
}

func (inspector *fakeInspector) LoopbackListeners(
	context.Context,
) ([]runtimeprobe.LoopbackListener, error) {
	return []runtimeprobe.LoopbackListener{{
		Address: netip.MustParseAddrPort("127.0.0.1:8080"),
	}}, nil
}
