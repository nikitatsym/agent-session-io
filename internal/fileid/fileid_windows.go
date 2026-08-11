//go:build windows

package fileid

import (
	"errors"
	"fmt"
	"syscall"
)

func token(path string) (identity string, err error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return Unavailable, fmt.Errorf("open %s for file identity: %w", path, err)
	}
	// The handle asks for no access at all, so a container the process may not
	// read still yields the identity the freshness gate compares.
	handle, err := syscall.CreateFile(
		name,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return Unavailable, fmt.Errorf("open %s for file identity: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, syscall.CloseHandle(handle))
	}()
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return Unavailable, fmt.Errorf("read file identity of %s: %w", path, err)
	}
	return fmt.Sprintf(
		"windows:%d:%d:%d",
		info.VolumeSerialNumber,
		info.FileIndexHigh,
		info.FileIndexLow,
	), nil
}
