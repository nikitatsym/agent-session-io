package presence

import (
	"context"
	"path/filepath"
	"testing"

	sessionio "github.com/nikitatsym/agent-session-io"
	"github.com/nikitatsym/agent-session-io/internal/runtimeprobe"
)

func TestCodexOpenFileProviderMapsExactValidatedLocator(t *testing.T) {
	home := t.TempDir()
	session := fixtureCodexFileSession(home)
	process := fixtureRuntimeProcess(42, fixtureProcessStart(), "codex", "")
	inspector := &fakeInspector{
		processes: []runtimeprobe.Process{process},
		fileUses: []runtimeprobe.FileUse{{
			Path:    filepath.Join(home, "sessions", "one.jsonl"),
			Process: process.Identity,
		}},
	}
	result := inspectCodexProvider(t, home, inspector, session)
	if len(result.Processes) != 1 ||
		len(result.Processes[0].Claims) != 1 ||
		result.Processes[0].Claims[0].NativeSessionID != "native-1" ||
		result.Processes[0].Claims[0].ExactSessionID != "session-1" {
		t.Fatalf("unexpected provider result: %#v", result)
	}
}

func TestCodexOpenFileProviderRejectsUnrelatedFileOwner(t *testing.T) {
	home := t.TempDir()
	session := fixtureCodexFileSession(home)
	start := fixtureProcessStart()
	codex := fixtureRuntimeProcess(42, start, "codex", "")
	other := fixtureRuntimeProcess(43, start, "editor", "")
	inspector := &fakeInspector{
		processes: []runtimeprobe.Process{codex, other},
		fileUses: []runtimeprobe.FileUse{{
			Path:    filepath.Join(home, "sessions", "one.jsonl"),
			Process: other.Identity,
		}},
	}
	result := inspectCodexProvider(t, home, inspector, session)
	if len(result.Processes) != 1 ||
		len(result.Processes[0].Claims) != 0 ||
		result.Processes[0].Reason != sessionio.PresenceReasonNoSessionIdentity {
		t.Fatalf("unexpected provider result: %#v", result)
	}
}

func TestCodexOpenFileProviderTypesUnavailableInspection(t *testing.T) {
	home := t.TempDir()
	start := fixtureProcessStart()
	inspector := &fakeInspector{
		processes:   []runtimeprobe.Process{fixtureRuntimeProcess(42, start, "codex", "")},
		fileUsesErr: runtimeprobe.ErrFileUseUnavailable,
	}
	result := inspectCodexProvider(t, home, inspector)
	if result.Status.Support != sessionio.PresenceSupportSupported ||
		result.Status.Capabilities[0].Support != sessionio.PresenceSupportUnavailable ||
		len(result.Processes) != 1 ||
		result.Processes[0].Reason != sessionio.PresenceReasonProviderUnavailable {
		t.Fatalf("unexpected provider result: %#v", result)
	}
}

func inspectCodexProvider(
	t *testing.T,
	home string,
	inspector runtimeprobe.Inspector,
	sessions ...sessionio.SessionRef,
) ProviderResult {
	t.Helper()
	provider, err := NewCodexOpenFileProvider(CodexProviderConfig{
		Home: home, Inspector: inspector,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Inspect(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fixtureCodexFileSession(home string) sessionio.SessionRef {
	session := fixtureSession(
		sessionio.HarnessCodex,
		"session-1",
		"occurrence-1",
		"native-1",
	)
	session.Occurrence.Locator.File.Root = home
	session.Occurrence.Locator.File.Path = "sessions/one.jsonl"
	return session
}
