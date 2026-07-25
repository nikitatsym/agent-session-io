//go:build darwin || linux

package runtimeprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxLsofFilesPerQuery = 128

type unixInspector struct {
	uid      uint64
	psPath   string
	lsofPath string
}

func NewInspector() (Inspector, error) {
	psPath, err := exec.LookPath("ps")
	if err != nil {
		return nil, fmt.Errorf(
			"%w: ps executable is unavailable: %v",
			ErrUnsupported,
			err,
		)
	}
	lsofPath, _ := exec.LookPath("lsof")
	return &unixInspector{
		uid:      uint64(os.Getuid()),
		psPath:   psPath,
		lsofPath: lsofPath,
	}, nil
}

func (inspector *unixInspector) Processes(ctx context.Context) ([]Process, error) {
	output, err := inspector.ps(ctx, "-axo", "uid=,pid=,lstart=,comm=")
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	processes, err := parsePSProcesses(output, inspector.uid)
	if err != nil {
		return nil, fmt.Errorf("parse process roster: %w", err)
	}
	refined := make([]Process, 0, len(processes))
	for index := range processes {
		process, ok := refineProcessCandidate(processes[index])
		if !ok {
			continue
		}
		processes[index] = process
		processes[index].ExecutablePath = platformExecutablePath(
			ctx,
			processes[index].Identity.PID,
			processes[index].Executable,
			inspector.lsofPath,
			false,
		)
		if processes[index].ExecutablePath != "" {
			processes[index].Executable = filepath.Base(processes[index].ExecutablePath)
		}
		refined = append(refined, processes[index])
	}
	return normalizeProcesses(refined), nil
}

func refineProcessCandidate(process Process) (Process, bool) {
	refined, err := platformRefineProcess(process)
	return refined, err == nil
}

func (inspector *unixInspector) Process(ctx context.Context, pid uint64) (Process, error) {
	if pid == 0 {
		return Process{}, ErrProcessNotFound
	}
	output, err := inspector.ps(
		ctx,
		"-p",
		strconv.FormatUint(pid, 10),
		"-o",
		"uid=,pid=,lstart=,comm=",
	)
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return Process{}, fmt.Errorf("%w: %v", ErrProcessNotFound, err)
		}
		return Process{}, fmt.Errorf("inspect process %d: %w", pid, err)
	}
	processes, err := parsePSProcesses(output, inspector.uid)
	if err != nil {
		return Process{}, fmt.Errorf("parse process %d: %w", pid, err)
	}
	if len(processes) == 0 {
		return Process{}, ErrProcessNotFound
	}
	process, err := platformRefineProcess(processes[0])
	if err != nil {
		return Process{}, fmt.Errorf("%w: %v", ErrProcessNotFound, err)
	}
	process.ExecutablePath = platformExecutablePath(
		ctx,
		pid,
		process.Executable,
		inspector.lsofPath,
		true,
	)
	if process.ExecutablePath != "" {
		process.Executable = filepath.Base(process.ExecutablePath)
	}
	return process, nil
}

func (inspector *unixInspector) ps(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, inspector.psPath, args...)
	command.Env = stableProcessEnvironment()
	return command.Output()
}

func stableProcessEnvironment() []string {
	environment := os.Environ()
	result := make([]string, 0, len(environment)+3)
	for _, value := range environment {
		if strings.HasPrefix(value, "LC_ALL=") ||
			strings.HasPrefix(value, "LANG=") ||
			strings.HasPrefix(value, "TZ=") {
			continue
		}
		result = append(result, value)
	}
	return append(result, "LC_ALL=C", "LANG=C", "TZ=UTC")
}

func parsePSProcesses(data []byte, uid uint64) ([]Process, error) {
	var processes []Process
	for lineNumber, line := range bytes.Split(data, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 8 {
			return nil, fmt.Errorf("line %d has %d fields, want at least 8", lineNumber+1, len(fields))
		}
		processUID, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d UID: %w", lineNumber+1, err)
		}
		if processUID < 0 || uint64(processUID) != uid {
			continue
		}
		pid, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d PID: %w", lineNumber+1, err)
		}
		startedAt, err := time.ParseInLocation(
			"Mon Jan 2 15:04:05 2006",
			strings.Join(fields[2:7], " "),
			time.UTC,
		)
		if err != nil {
			return nil, fmt.Errorf("line %d start time: %w", lineNumber+1, err)
		}
		executable := strings.Join(fields[7:], " ")
		processes = append(processes, Process{
			Identity:   ProcessIdentity{PID: pid, StartedAt: startedAt},
			Executable: filepath.Base(executable),
			ExecutablePath: func() string {
				if filepath.IsAbs(executable) {
					return executable
				}
				return ""
			}(),
		})
	}
	return processes, nil
}

func (inspector *unixInspector) FileUses(
	ctx context.Context,
	paths []string,
) ([]FileUse, error) {
	if len(paths) == 0 {
		return []FileUse{}, nil
	}
	if inspector.lsofPath == "" {
		return nil, fmt.Errorf("%w: lsof executable is unavailable", ErrFileUseUnavailable)
	}
	before, err := inspector.Processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample processes before file inspection: %w", err)
	}
	beforeByPID := processesByPID(before)
	literalByClean := make(map[string][]string, len(paths))
	cleanPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("candidate file path is empty")
		}
		clean := canonicalUnixPath(path)
		if _, exists := literalByClean[clean]; !exists {
			cleanPaths = append(cleanPaths, clean)
		}
		literalByClean[clean] = append(literalByClean[clean], path)
	}
	slices.Sort(cleanPaths)

	var uses []FileUse
	for start := 0; start < len(cleanPaths); start += maxLsofFilesPerQuery {
		end := min(start+maxLsofFilesPerQuery, len(cleanPaths))
		observed, err := inspector.lsofFileOwners(ctx, cleanPaths[start:end])
		if err != nil {
			return nil, err
		}
		for path, pids := range observed {
			for _, pid := range pids {
				beforeProcess, found := beforeByPID[pid]
				if !found {
					continue
				}
				afterProcess, err := inspector.Process(ctx, pid)
				if err != nil || afterProcess.Identity != beforeProcess.Identity {
					continue
				}
				for _, literal := range literalByClean[path] {
					uses = append(uses, FileUse{
						Path:    literal,
						Process: afterProcess.Identity,
					})
				}
			}
		}
	}
	return normalizeFileUses(uses), nil
}

func (inspector *unixInspector) lsofFileOwners(
	ctx context.Context,
	paths []string,
) (map[string][]uint64, error) {
	arguments := []string{"-nP", "-Fpn", "--"}
	arguments = append(arguments, paths...)
	output, err := inspector.runLsof(ctx, arguments...)
	if err != nil {
		return nil, fmt.Errorf("inspect exact file uses: %w", err)
	}
	owners := make(map[string][]uint64)
	var pid uint64
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			value, parseErr := strconv.ParseUint(string(line[1:]), 10, 64)
			if parseErr != nil {
				return nil, fmt.Errorf("parse lsof process field %q: %w", line, parseErr)
			}
			pid = value
		case 'n':
			if pid == 0 {
				return nil, fmt.Errorf("lsof file field %q has no process", line)
			}
			path := canonicalUnixPath(string(line[1:]))
			if _, candidate := owners[path]; candidate {
				owners[path] = append(owners[path], pid)
				continue
			}
			for _, requested := range paths {
				if path == requested {
					owners[path] = append(owners[path], pid)
					break
				}
			}
		}
	}
	for path := range owners {
		slices.Sort(owners[path])
		owners[path] = slices.Compact(owners[path])
	}
	return owners, nil
}

func canonicalUnixPath(path string) string {
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return clean
}

func (inspector *unixInspector) LoopbackListeners(
	ctx context.Context,
) ([]LoopbackListener, error) {
	if inspector.lsofPath == "" {
		return nil, fmt.Errorf("%w: lsof executable is unavailable", ErrUnsupported)
	}
	before, err := inspector.Processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample processes before listener inspection: %w", err)
	}
	beforeByPID := processesByPID(before)
	output, err := inspector.runLsof(
		ctx,
		"-nP",
		"-a",
		"-iTCP",
		"-sTCP:LISTEN",
		"-Fpn",
	)
	if err != nil {
		return nil, fmt.Errorf("inspect loopback listeners: %w", err)
	}
	observed, err := parseLsofListeners(output)
	if err != nil {
		return nil, err
	}
	var listeners []LoopbackListener
	for _, owner := range observed {
		beforeProcess, found := beforeByPID[owner.pid]
		if !found {
			continue
		}
		afterProcess, err := inspector.Process(ctx, owner.pid)
		if err != nil || afterProcess.Identity != beforeProcess.Identity {
			continue
		}
		listeners = append(listeners, LoopbackListener{
			Network: owner.network,
			Address: owner.address,
			Process: afterProcess.Identity,
		})
	}
	return normalizeListeners(listeners), nil
}

func (inspector *unixInspector) runLsof(
	ctx context.Context,
	arguments ...string,
) ([]byte, error) {
	command := exec.CommandContext(ctx, inspector.lsofPath, arguments...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 &&
		stderr.Len() == 0 {
		return output, nil
	}
	if stderr.Len() > 0 {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil, err
}

func processesByPID(processes []Process) map[uint64]Process {
	result := make(map[uint64]Process, len(processes))
	for _, process := range processes {
		result[process.Identity.PID] = process
	}
	return result
}

type unixListenerOwner struct {
	pid     uint64
	network string
	address netip.AddrPort
}

func parseLsofListeners(data []byte) ([]unixListenerOwner, error) {
	var result []unixListenerOwner
	var pid uint64
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			value, err := strconv.ParseUint(string(line[1:]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse lsof process field %q: %w", line, err)
			}
			pid = value
		case 'n':
			if pid == 0 {
				return nil, fmt.Errorf("lsof listener field %q has no process", line)
			}
			name := strings.TrimSpace(string(line[1:]))
			if space := strings.IndexByte(name, ' '); space >= 0 {
				name = name[:space]
			}
			address, err := netip.ParseAddrPort(name)
			if err != nil || !address.Addr().IsLoopback() {
				continue
			}
			network := "tcp6"
			if address.Addr().Is4() {
				network = "tcp4"
			}
			result = append(result, unixListenerOwner{
				pid:     pid,
				network: network,
				address: address,
			})
		}
	}
	return result, nil
}
