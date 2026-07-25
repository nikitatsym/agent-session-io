//go:build linux

package runtimeprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func platformRefineProcess(process Process) (Process, error) {
	path := filepath.Join(
		"/proc",
		strconv.FormatUint(process.Identity.PID, 10),
		"stat",
	)
	data, err := os.ReadFile(path)
	if err != nil {
		return Process{}, err
	}
	line := strings.TrimSpace(string(data))
	closeParen := strings.LastIndexByte(line, ')')
	if closeParen < 0 || closeParen+2 >= len(line) {
		return Process{}, errors.New("malformed Linux process stat")
	}
	fields := strings.Fields(line[closeParen+2:])
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return Process{}, errors.New("Linux process stat has no start time")
	}
	token, err := strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return Process{}, err
	}
	process.Identity.token = token
	return process, nil
}

func platformExecutablePath(
	_ context.Context,
	pid uint64,
	fallback string,
	_ string,
	_ bool,
) string {
	path, err := os.Readlink(filepath.Join("/proc", strconv.FormatUint(pid, 10), "exe"))
	if err == nil {
		return path
	}
	if filepath.IsAbs(fallback) {
		return fallback
	}
	return ""
}
