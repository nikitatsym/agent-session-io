//go:build darwin || linux

package runtimeprobe

import (
	"context"
	"os"
	"testing"
)

func TestUnixInspectorFindsCurrentProcessHeldFileAndListener(t *testing.T) {
	inspector, current := liveInspectorAndCurrentProcess(t)
	if current.Identity.PID != uint64(os.Getpid()) || current.Executable == "" {
		t.Fatalf("unexpected current process: %#v", current)
	}

	processes, err := inspector.Processes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundCurrent := false
	for _, process := range processes {
		if process.Identity == current.Identity {
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		t.Fatalf("current process %#v not found in roster", current.Identity)
	}

	assertLiveFileOwnership(t, inspector, current)
	assertLiveLoopbackListener(t, inspector, current)
}
