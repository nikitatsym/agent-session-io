//go:build windows

package runtimeprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

func TestWindowsNativeLayouts(t *testing.T) {
	if size := unsafe.Sizeof(rmUniqueProcess{}); size != 12 {
		t.Fatalf("RM_UNIQUE_PROCESS size = %d, want 12", size)
	}
	if size := unsafe.Sizeof(rmProcessInfo{}); size != rmProcessInfoSize {
		t.Fatalf("RM_PROCESS_INFO size = %d, want %d", size, rmProcessInfoSize)
	}
	if offset := unsafe.Offsetof(rmProcessInfo{}.ApplicationName); offset != 12 {
		t.Fatalf("RM_PROCESS_INFO strAppName offset = %d, want 12", offset)
	}
	if offset := unsafe.Offsetof(rmProcessInfo{}.Restartable); offset != 664 {
		t.Fatalf("RM_PROCESS_INFO bRestartable offset = %d, want 664", offset)
	}
}

func TestParseWindowsTCPListenerTables(t *testing.T) {
	ipv4 := make([]byte, 4+mibTCPRowOwnerPIDSize)
	binary.LittleEndian.PutUint32(ipv4[:4], 1)
	row4 := ipv4[4:]
	binary.LittleEndian.PutUint32(row4[0:4], mibTCPStateListen)
	copy(row4[4:8], []byte{127, 0, 0, 1})
	binary.BigEndian.PutUint16(row4[8:10], 8080)
	binary.LittleEndian.PutUint32(row4[20:24], 42)

	owners, err := parseTCPListeners(ipv4, addressFamilyINET)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 ||
		owners[0].network != "tcp4" ||
		owners[0].address.String() != "127.0.0.1:8080" ||
		owners[0].pid != 42 {
		t.Fatalf("unexpected IPv4 listener: %#v", owners)
	}

	ipv6 := make([]byte, 4+mibTCP6RowOwnerPIDSize)
	binary.LittleEndian.PutUint32(ipv6[:4], 1)
	row6 := ipv6[4:]
	row6[15] = 1
	binary.BigEndian.PutUint16(row6[20:22], 9090)
	binary.LittleEndian.PutUint32(row6[48:52], mibTCPStateListen)
	binary.LittleEndian.PutUint32(row6[52:56], 84)

	owners, err = parseTCPListeners(ipv6, addressFamilyINET6)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 ||
		owners[0].network != "tcp6" ||
		owners[0].address.String() != "[::1]:9090" ||
		owners[0].pid != 84 {
		t.Fatalf("unexpected IPv6 listener: %#v", owners)
	}
}

func TestCanonicalWindowsPath(t *testing.T) {
	normal := canonicalWindowsPath(`C:\Users\Ari\sessions\one.jsonl`)
	extended := canonicalWindowsPath(`\\?\c:\users\ari\sessions\one.jsonl`)
	if normal != extended {
		t.Fatalf("canonical paths differ: %q and %q", normal, extended)
	}
	unc := canonicalWindowsPath(`\\?\UNC\server\share\sessions\one.jsonl`)
	if unc != `\\server\share\sessions\one.jsonl` {
		t.Fatalf("unexpected canonical UNC path: %q", unc)
	}
}

func TestWindowsInspectorFindsCurrentProcessAndListener(t *testing.T) {
	inspector, current := liveInspectorAndCurrentProcess(t)
	if current.Executable != filepath.Base(current.ExecutablePath) || current.ExecutablePath == "" {
		t.Fatalf("unexpected executable identity: %#v", current)
	}
	assertLiveLoopbackListener(t, inspector, current)
}

func TestWindowsRestartManagerMapsHeldFile(t *testing.T) {
	inspector, current := liveInspectorAndCurrentProcess(t)
	assertLiveFileOwnership(t, inspector, current)
}

// A context cancelled up front stops at the entry checks, so this pins the
// entry contract only; the per-row confirmation loops are not reached.
func TestWindowsInspectorRejectsCancelledContext(t *testing.T) {
	inspector, err := NewInspector()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := inspector.Processes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Processes error = %v, want context.Canceled", err)
	}
	if _, err := inspector.Process(ctx, uint64(os.Getpid())); !errors.Is(err, context.Canceled) {
		t.Fatalf("Process error = %v, want context.Canceled", err)
	}
	if _, err := inspector.FileUses(ctx, []string{`C:\sessions\one.jsonl`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("FileUses error = %v, want context.Canceled", err)
	}
	if _, err := inspector.LoopbackListeners(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoopbackListeners error = %v, want context.Canceled", err)
	}
}

func TestMapExactFileOwnersPrunesEmptyBatches(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	owner := ProcessIdentity{PID: 12, StartedAt: start}
	paths := make([]string, 100)
	owned := map[string]bool{"file-2": true, "file-77": true}
	for index := range paths {
		paths[index] = fmt.Sprintf("file-%d", index)
	}
	queries := 0
	result, err := mapExactFileOwners(
		context.Background(),
		paths,
		100,
		64,
		func(_ context.Context, batch []string) ([]ProcessIdentity, error) {
			queries++
			for _, path := range batch {
				if owned[path] {
					return []ProcessIdentity{owner}, nil
				}
			}
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || len(result["file-2"]) != 1 || len(result["file-77"]) != 1 {
		t.Fatalf("unexpected exact mapping: %#v", result)
	}
	if queries >= len(paths) {
		t.Fatalf("batch pruning used %d queries for %d candidates", queries, len(paths))
	}
}

func TestMapExactFileOwnersReturnsUnavailableAtQueryBound(t *testing.T) {
	paths := []string{"a", "b", "c", "d"}
	_, err := mapExactFileOwners(
		context.Background(),
		paths,
		len(paths),
		2,
		func(context.Context, []string) ([]ProcessIdentity, error) {
			return []ProcessIdentity{{PID: 12, StartedAt: time.Now()}}, nil
		},
	)
	if !errors.Is(err, ErrFileUseUnavailable) {
		t.Fatalf("got %v, want ErrFileUseUnavailable", err)
	}
}
