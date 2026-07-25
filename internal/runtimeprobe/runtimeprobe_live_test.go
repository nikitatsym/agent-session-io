package runtimeprobe

import (
	"context"
	"net"
	"os"
	"testing"
)

func liveInspectorAndCurrentProcess(t *testing.T) (Inspector, Process) {
	t.Helper()
	inspector, err := NewInspector()
	if err != nil {
		t.Fatal(err)
	}
	current, err := inspector.Process(context.Background(), uint64(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	return inspector, current
}

func assertLiveFileOwnership(
	t *testing.T,
	inspector Inspector,
	current Process,
) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "runtimeprobe-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	uses, err := inspector.FileUses(context.Background(), []string{file.Name()})
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range uses {
		if use.Path == file.Name() && use.Process == current.Identity {
			return
		}
	}
	t.Fatalf(
		"held file %q owned by %#v not found in %#v",
		file.Name(),
		current.Identity,
		uses,
	)
}

func assertLiveLoopbackListener(
	t *testing.T,
	inspector Inspector,
	current Process,
) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listeners, err := inspector.LoopbackListeners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	for _, observed := range listeners {
		if observed.Network == "tcp4" &&
			observed.Address.String() == address &&
			observed.Process == current.Identity {
			return
		}
	}
	t.Fatalf(
		"listener %s owned by %#v not found in %#v",
		address,
		current.Identity,
		listeners,
	)
}
