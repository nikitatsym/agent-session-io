//go:build darwin

package runtimeprobe

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func platformRefineProcess(process Process) (Process, error) {
	if process.Identity.PID > uint64(^uint32(0)>>1) {
		return Process{}, ErrProcessNotFound
	}
	info, err := unix.SysctlKinfoProc(
		"kern.proc.pid",
		int(process.Identity.PID),
	)
	if err != nil {
		return Process{}, err
	}
	if info.Proc.P_pid <= 0 ||
		uint64(info.Proc.P_pid) != process.Identity.PID {
		return Process{}, errors.New("Darwin process identity changed")
	}
	start := info.Proc.P_starttime
	process.Identity.StartedAt = time.Unix(
		start.Sec,
		int64(start.Usec)*int64(time.Microsecond),
	).UTC()
	process.Identity.token = uint64(start.Sec)*1_000_000 + uint64(start.Usec)
	return process, nil
}

func platformExecutablePath(
	ctx context.Context,
	pid uint64,
	fallback string,
	lsofPath string,
	resolve bool,
) string {
	if filepath.IsAbs(fallback) {
		return fallback
	}
	if !resolve || lsofPath == "" {
		return ""
	}
	command := exec.CommandContext(
		ctx,
		lsofPath,
		"-a",
		"-p",
		strconv.FormatUint(pid, 10),
		"-d",
		"txt",
		"-Fn",
	)
	// The full image path is optional for non-version-named processes.
	output, _ := command.Output()
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if len(line) > 1 && line[0] == 'n' && filepath.IsAbs(string(line[1:])) {
			return string(line[1:])
		}
	}
	return ""
}
