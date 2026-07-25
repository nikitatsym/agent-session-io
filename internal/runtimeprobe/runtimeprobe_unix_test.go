//go:build darwin || linux

package runtimeprobe

import (
	"testing"
)

func TestParsePSProcessesFiltersUserAndPreservesStartIdentity(t *testing.T) {
	processes, err := parsePSProcesses([]byte(
		"501 42 Sat Jul 25 10:11:12 2026 /usr/local/bin/claude\n"+
			"502 43 Sat Jul 25 10:11:13 2026 /usr/local/bin/codex\n",
	), 501)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 ||
		processes[0].Identity.PID != 42 ||
		processes[0].Identity.StartedAt.Format("2006-01-02T15:04:05Z07:00") !=
			"2026-07-25T10:11:12Z" ||
		processes[0].Executable != "claude" ||
		processes[0].ExecutablePath != "/usr/local/bin/claude" {
		t.Fatalf("unexpected processes: %#v", processes)
	}
}

func TestParseLsofListenersKeepsOnlyNumericLoopback(t *testing.T) {
	listeners, err := parseLsofListeners([]byte(
		"p42\n" +
			"n127.0.0.1:8080\n" +
			"n*:8081\n" +
			"p43\n" +
			"n[::1]:9090\n" +
			"n10.0.0.1:9091\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 2 ||
		listeners[0].network != "tcp4" ||
		listeners[0].address.String() != "127.0.0.1:8080" ||
		listeners[1].network != "tcp6" ||
		listeners[1].address.String() != "[::1]:9090" {
		t.Fatalf("unexpected listeners: %#v", listeners)
	}
}
