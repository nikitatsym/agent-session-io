//go:build windows

package runtimeprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	maxRMFilesPerSession = 1024
	maxRMQueries         = 512
	maxRMProcesses       = 4096
	rmSessionKeySize     = 33

	tcpTableOwnerPIDListener = 3
	mibTCPStateListen        = 2
	addressFamilyINET        = 2
	addressFamilyINET6       = 23

	mibTCPRowOwnerPIDSize  = 24
	mibTCP6RowOwnerPIDSize = 56
	rmProcessInfoSize      = 668
)

var (
	restartManagerDLL  = windows.NewLazySystemDLL("rstrtmgr.dll")
	rmStartSessionProc = restartManagerDLL.NewProc("RmStartSession")
	rmRegisterProc     = restartManagerDLL.NewProc("RmRegisterResources")
	rmGetListProc      = restartManagerDLL.NewProc("RmGetList")
	rmEndSessionProc   = restartManagerDLL.NewProc("RmEndSession")

	ipHelperDLL             = windows.NewLazySystemDLL("iphlpapi.dll")
	getExtendedTCPTableProc = ipHelperDLL.NewProc("GetExtendedTcpTable")
)

type windowsInspector struct {
	userSID *windows.SID
}

type rmUniqueProcess struct {
	ProcessID uint32
	StartedAt windows.Filetime
}

type rmProcessInfo struct {
	Process           rmUniqueProcess
	ApplicationName   [256]uint16
	ServiceShortName  [64]uint16
	ApplicationType   uint32
	ApplicationStatus uint32
	TerminalSessionID uint32
	Restartable       int32
}

type listenerOwner struct {
	network string
	address netip.AddrPort
	pid     uint32
}

func NewInspector() (Inspector, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("get current process user: %w", err)
	}
	sid, err := user.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy current process user SID: %w", err)
	}
	return &windowsInspector{userSID: sid}, nil
}

func (inspector *windowsInspector) Processes(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var processes []Process
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		process, processErr := inspector.process(uint64(entry.ProcessID))
		if processErr == nil {
			processes = append(processes, process)
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return normalizeProcesses(processes), nil
}

func (inspector *windowsInspector) Process(ctx context.Context, pid uint64) (Process, error) {
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	process, err := inspector.process(pid)
	if err != nil {
		return Process{}, err
	}
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	return process, nil
}

func (inspector *windowsInspector) process(pid uint64) (Process, error) {
	if pid == 0 || pid > math.MaxUint32 {
		return Process{}, ErrProcessNotFound
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return Process{}, ErrProcessNotFound
		}
		return Process{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		return Process{}, fmt.Errorf("open process %d token: %w", pid, err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return Process{}, fmt.Errorf("get process %d user: %w", pid, err)
	}
	if !inspector.userSID.Equals(user.User.Sid) {
		return Process{}, ErrProcessNotSameUser
	}

	var creation, exit, kernel, userTime windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &userTime); err != nil {
		return Process{}, fmt.Errorf("get process %d times: %w", pid, err)
	}
	image, err := queryProcessImage(handle)
	if err != nil {
		return Process{}, fmt.Errorf("get process %d image: %w", pid, err)
	}
	return Process{
		Identity: ProcessIdentity{
			PID:       pid,
			StartedAt: time.Unix(0, creation.Nanoseconds()).UTC(),
		},
		Executable:     filepath.Base(image),
		ExecutablePath: image,
	}, nil
}

func queryProcessImage(handle windows.Handle) (string, error) {
	for size := uint32(windows.MAX_PATH); size <= 32768; size *= 2 {
		buffer := make([]uint16, size)
		length := size
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", err
		}
	}
	return "", windows.ERROR_INSUFFICIENT_BUFFER
}

func (inspector *windowsInspector) FileUses(ctx context.Context, paths []string) ([]FileUse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return []FileUse{}, nil
	}
	before, err := inspector.Processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample processes before file inspection: %w", err)
	}
	beforeByPID := make(map[uint64]Process, len(before))
	for _, process := range before {
		beforeByPID[process.Identity.PID] = process
	}

	literalByCanonical := make(map[string][]string, len(paths))
	registerByCanonical := make(map[string]string, len(paths))
	var canonicalOrder []string
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("candidate file path is empty")
		}
		canonical := canonicalWindowsPath(path)
		if _, ok := literalByCanonical[canonical]; !ok {
			canonicalOrder = append(canonicalOrder, canonical)
			registerByCanonical[canonical] = path
		}
		literalByCanonical[canonical] = append(literalByCanonical[canonical], path)
	}
	slices.Sort(canonicalOrder)

	ownersByPath, err := mapExactFileOwners(
		ctx,
		canonicalOrder,
		maxRMFilesPerSession,
		maxRMQueries,
		func(ctx context.Context, batch []string) ([]ProcessIdentity, error) {
			registerPaths := make([]string, len(batch))
			for index, canonical := range batch {
				registerPaths[index] = registerByCanonical[canonical]
			}
			owners, err := restartManagerOwners(ctx, registerPaths)
			if err != nil {
				return nil, err
			}
			identities := make([]ProcessIdentity, 0, len(owners))
			for _, owner := range owners {
				beforeProcess, ok := beforeByPID[uint64(owner.ProcessID)]
				if ok && sameRMIdentity(beforeProcess.Identity, owner) {
					identities = append(identities, beforeProcess.Identity)
				}
			}
			return identities, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("map exact file uses: %w", err)
	}

	var uses []FileUse
	for _, canonical := range canonicalOrder {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		literals := literalByCanonical[canonical]
		for _, owner := range ownersByPath[canonical] {
			afterProcess, err := inspector.Process(ctx, owner.PID)
			if err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return nil, contextErr
				}
				continue
			}
			if afterProcess.Identity != owner {
				continue
			}
			for _, literal := range literals {
				uses = append(uses, FileUse{Path: literal, Process: afterProcess.Identity})
			}
		}
	}
	return normalizeFileUses(uses), nil
}

func restartManagerOwners(ctx context.Context, paths []string) (owners []rmUniqueProcess, err error) {
	if len(paths) == 0 || uint64(len(paths)) > math.MaxUint32 {
		return nil, errors.New("Restart Manager file batch size is invalid")
	}
	key := make([]uint16, rmSessionKeySize)
	var session uint32
	if err := rmCall("RmStartSession", rmStartSessionProc,
		uintptr(unsafe.Pointer(&session)),
		0,
		uintptr(unsafe.Pointer(&key[0])),
	); err != nil {
		return nil, err
	}
	defer func() {
		endErr := rmCall("RmEndSession", rmEndSessionProc, uintptr(session))
		err = errors.Join(err, endErr)
	}()

	files := make([]*uint16, len(paths))
	for index, path := range paths {
		files[index], err = windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, fmt.Errorf("encode candidate file %q: %w", path, err)
		}
	}
	if err := rmCall("RmRegisterResources", rmRegisterProc,
		uintptr(session),
		uintptr(len(files)),
		uintptr(unsafe.Pointer(&files[0])),
		0,
		0,
		0,
		0,
	); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var needed, count, rebootReasons uint32
	result, _, _ := rmGetListProc.Call(
		uintptr(session),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&count)),
		0,
		uintptr(unsafe.Pointer(&rebootReasons)),
	)
	if result == 0 {
		return nil, nil
	}
	if syscall.Errno(result) != windows.ERROR_MORE_DATA {
		return nil, fmt.Errorf("RmGetList: %w", syscall.Errno(result))
	}

	for attempts := 0; attempts < 4; attempts++ {
		if needed == 0 {
			return nil, errors.New("RmGetList requested an empty retry buffer")
		}
		if needed > maxRMProcesses {
			return nil, fmt.Errorf("RmGetList reported %d processes, limit is %d", needed, maxRMProcesses)
		}
		infos := make([]rmProcessInfo, needed)
		count = needed
		result, _, _ = rmGetListProc.Call(
			uintptr(session),
			uintptr(unsafe.Pointer(&needed)),
			uintptr(unsafe.Pointer(&count)),
			uintptr(unsafe.Pointer(&infos[0])),
			uintptr(unsafe.Pointer(&rebootReasons)),
		)
		if result == 0 {
			owners = make([]rmUniqueProcess, count)
			for index := range owners {
				owners[index] = infos[index].Process
			}
			return owners, nil
		}
		if syscall.Errno(result) != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("RmGetList: %w", syscall.Errno(result))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("RmGetList process roster did not stabilize")
}

func rmCall(name string, procedure *windows.LazyProc, arguments ...uintptr) error {
	result, _, _ := procedure.Call(arguments...)
	if result != 0 {
		return fmt.Errorf("%s: %w", name, syscall.Errno(result))
	}
	return nil
}

func sameRMIdentity(identity ProcessIdentity, process rmUniqueProcess) bool {
	return identity.PID == uint64(process.ProcessID) &&
		identity.StartedAt.Equal(time.Unix(0, process.StartedAt.Nanoseconds()).UTC())
}

func canonicalWindowsPath(path string) string {
	canonical := path
	if strings.HasPrefix(strings.ToUpper(canonical), `\\?\UNC\`) {
		canonical = `\\` + canonical[len(`\\?\UNC\`):]
	} else if strings.HasPrefix(canonical, `\\?\`) {
		canonical = canonical[len(`\\?\`):]
	}
	return strings.ToLower(filepath.Clean(canonical))
}

func (inspector *windowsInspector) LoopbackListeners(ctx context.Context) ([]LoopbackListener, error) {
	before, err := inspector.Processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("sample processes before listener inspection: %w", err)
	}
	beforeByPID := make(map[uint64]Process, len(before))
	for _, process := range before {
		beforeByPID[process.Identity.PID] = process
	}

	var owners []listenerOwner
	for _, family := range []uint32{addressFamilyINET, addressFamilyINET6} {
		table, err := extendedTCPTable(ctx, family)
		if err != nil {
			return nil, err
		}
		parsed, err := parseTCPListeners(table, family)
		if err != nil {
			return nil, err
		}
		owners = append(owners, parsed...)
	}

	afterByPID := make(map[uint64]Process)
	var listeners []LoopbackListener
	for _, owner := range owners {
		beforeProcess, ok := beforeByPID[uint64(owner.pid)]
		if !ok || !owner.address.Addr().IsLoopback() {
			continue
		}
		afterProcess, ok := afterByPID[uint64(owner.pid)]
		if !ok {
			afterProcess, err = inspector.Process(ctx, uint64(owner.pid))
			if err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return nil, contextErr
				}
				continue
			}
			afterByPID[uint64(owner.pid)] = afterProcess
		}
		if afterProcess.Identity != beforeProcess.Identity {
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

func extendedTCPTable(ctx context.Context, family uint32) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var size uint32
	result, _, _ := getExtendedTCPTableProc.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(family),
		tcpTableOwnerPIDListener,
		0,
	)
	if syscall.Errno(result) != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, fmt.Errorf("GetExtendedTcpTable size for family %d: %w", family, syscall.Errno(result))
	}
	if size < 4 {
		return nil, fmt.Errorf("GetExtendedTcpTable family %d reported invalid size %d", family, size)
	}
	buffer := make([]byte, size)
	result, _, _ = getExtendedTCPTableProc.Call(
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(family),
		tcpTableOwnerPIDListener,
		0,
	)
	if result != 0 {
		return nil, fmt.Errorf("GetExtendedTcpTable family %d: %w", family, syscall.Errno(result))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return buffer[:size], nil
}

func parseTCPListeners(table []byte, family uint32) ([]listenerOwner, error) {
	if len(table) < 4 {
		return nil, errors.New("TCP table is shorter than its row count")
	}
	count := int(binary.LittleEndian.Uint32(table[:4]))
	rowSize := 0
	switch family {
	case addressFamilyINET:
		rowSize = mibTCPRowOwnerPIDSize
	case addressFamilyINET6:
		rowSize = mibTCP6RowOwnerPIDSize
	default:
		return nil, fmt.Errorf("unsupported TCP address family %d", family)
	}
	if count > (len(table)-4)/rowSize {
		return nil, fmt.Errorf("TCP table declares %d rows in %d bytes", count, len(table))
	}

	owners := make([]listenerOwner, 0, count)
	for index := 0; index < count; index++ {
		row := table[4+index*rowSize : 4+(index+1)*rowSize]
		var state, pid uint32
		var address netip.Addr
		var port uint16
		var network string
		if family == addressFamilyINET {
			state = binary.LittleEndian.Uint32(row[0:4])
			address = netip.AddrFrom4([4]byte(row[4:8]))
			port = binary.BigEndian.Uint16(row[8:10])
			pid = binary.LittleEndian.Uint32(row[20:24])
			network = "tcp4"
		} else {
			address, _ = netip.AddrFromSlice(row[0:16])
			port = binary.BigEndian.Uint16(row[20:22])
			state = binary.LittleEndian.Uint32(row[48:52])
			pid = binary.LittleEndian.Uint32(row[52:56])
			network = "tcp6"
		}
		if state != mibTCPStateListen || !address.IsValid() {
			continue
		}
		owners = append(owners, listenerOwner{
			network: network,
			address: netip.AddrPortFrom(address, port),
			pid:     pid,
		})
	}
	return owners, nil
}
