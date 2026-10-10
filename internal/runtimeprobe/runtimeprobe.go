package runtimeprobe

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"
)

var (
	ErrUnsupported        = errors.New("runtime inspection is unsupported")
	ErrProcessNotFound    = errors.New("process not found")
	ErrProcessNotSameUser = errors.New("process does not belong to the current user")
	ErrFileUseUnavailable = errors.New("exact file-use inspection is unavailable")
)

type ProcessIdentity struct {
	PID       uint64
	StartedAt time.Time
	token     uint64
}

type Process struct {
	Identity       ProcessIdentity
	Executable     string
	ExecutablePath string
}

type FileUse struct {
	Path    string
	Process ProcessIdentity
}

type LoopbackListener struct {
	Network string
	Address netip.AddrPort
	Process ProcessIdentity
}

type Inspector interface {
	Processes(context.Context) ([]Process, error)
	Process(context.Context, uint64) (Process, error)
	FileUses(context.Context, []string) ([]FileUse, error)
	LoopbackListeners(context.Context) ([]LoopbackListener, error)
}

func normalizeProcesses(processes []Process) []Process {
	result := slices.Clone(processes)
	for index := range result {
		result[index].Identity.StartedAt = result[index].Identity.StartedAt.UTC()
	}
	slices.SortFunc(result, compareProcess)
	return slices.CompactFunc(result, func(left, right Process) bool {
		return compareProcess(left, right) == 0
	})
}

func compareProcess(left, right Process) int {
	if order := compareIdentity(left.Identity, right.Identity); order != 0 {
		return order
	}
	if left.Executable < right.Executable {
		return -1
	}
	if left.Executable > right.Executable {
		return 1
	}
	if left.ExecutablePath < right.ExecutablePath {
		return -1
	}
	if left.ExecutablePath > right.ExecutablePath {
		return 1
	}
	return 0
}

func normalizeFileUses(uses []FileUse) []FileUse {
	result := slices.Clone(uses)
	for index := range result {
		result[index].Process.StartedAt = result[index].Process.StartedAt.UTC()
	}
	slices.SortFunc(result, func(left, right FileUse) int {
		if left.Path < right.Path {
			return -1
		}
		if left.Path > right.Path {
			return 1
		}
		return compareIdentity(left.Process, right.Process)
	})
	return slices.CompactFunc(result, func(left, right FileUse) bool {
		return left.Path == right.Path && compareIdentity(left.Process, right.Process) == 0
	})
}

func normalizeListeners(listeners []LoopbackListener) []LoopbackListener {
	result := slices.Clone(listeners)
	for index := range result {
		result[index].Process.StartedAt = result[index].Process.StartedAt.UTC()
	}
	slices.SortFunc(result, func(left, right LoopbackListener) int {
		if left.Network < right.Network {
			return -1
		}
		if left.Network > right.Network {
			return 1
		}
		if order := left.Address.Compare(right.Address); order != 0 {
			return order
		}
		return compareIdentity(left.Process, right.Process)
	})
	return slices.CompactFunc(result, func(left, right LoopbackListener) bool {
		return left.Network == right.Network &&
			left.Address == right.Address &&
			compareIdentity(left.Process, right.Process) == 0
	})
}

func compareIdentity(left, right ProcessIdentity) int {
	if left.PID < right.PID {
		return -1
	}
	if left.PID > right.PID {
		return 1
	}
	if order := left.StartedAt.Compare(right.StartedAt); order != 0 {
		return order
	}
	if left.token < right.token {
		return -1
	}
	if left.token > right.token {
		return 1
	}
	return 0
}
