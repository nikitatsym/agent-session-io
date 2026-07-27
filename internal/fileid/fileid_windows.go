//go:build windows

package fileid

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func token(path string) (identity string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return Unavailable, fmt.Errorf("open %s for file identity: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(
		syscall.Handle(file.Fd()),
		&info,
	); err != nil {
		return Unavailable, fmt.Errorf("read file identity of %s: %w", path, err)
	}
	return fmt.Sprintf(
		"windows:%d:%d:%d",
		info.VolumeSerialNumber,
		info.FileIndexHigh,
		info.FileIndexLow,
	), nil
}
