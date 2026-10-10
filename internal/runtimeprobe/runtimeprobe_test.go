package runtimeprobe

import (
	"net/netip"
	"testing"
	"time"
)

func TestNormalizeProcessesOrdersAndDeduplicates(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	processes := normalizeProcesses([]Process{
		{Identity: ProcessIdentity{PID: 12, StartedAt: start}, Executable: "codex.exe"},
		{Identity: ProcessIdentity{PID: 7, StartedAt: start}, Executable: "claude.exe"},
		{Identity: ProcessIdentity{PID: 12, StartedAt: start.UTC()}, Executable: "codex.exe"},
	})

	if len(processes) != 2 {
		t.Fatalf("got %d processes, want 2", len(processes))
	}
	if processes[0].Identity.PID != 7 || processes[1].Identity.PID != 12 {
		t.Fatalf("unexpected process order: %#v", processes)
	}
	if processes[1].Identity.StartedAt.Location() != time.UTC {
		t.Fatalf("start time was not normalized to UTC: %v", processes[1].Identity.StartedAt)
	}
}

func TestNormalizeEvidenceOrdersAndDeduplicates(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	identity := ProcessIdentity{PID: 12, StartedAt: start}
	uses := normalizeFileUses([]FileUse{
		{Path: `C:\b.jsonl`, Process: identity},
		{Path: `C:\a.jsonl`, Process: identity},
		{Path: `C:\a.jsonl`, Process: identity},
	})
	if len(uses) != 2 || uses[0].Path != `C:\a.jsonl` {
		t.Fatalf("unexpected file uses: %#v", uses)
	}

	address := netip.MustParseAddrPort("[::1]:8080")
	listeners := normalizeListeners([]LoopbackListener{
		{Network: "tcp6", Address: address, Process: identity},
		{Network: "tcp6", Address: address, Process: identity},
	})
	if len(listeners) != 1 {
		t.Fatalf("unexpected listeners: %#v", listeners)
	}
}
