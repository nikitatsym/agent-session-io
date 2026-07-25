//go:build windows

package runtimeprobe

import (
	"encoding/binary"
	"path/filepath"
	"testing"
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
