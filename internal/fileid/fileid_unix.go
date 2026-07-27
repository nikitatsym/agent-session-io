//go:build unix

package fileid

import (
	"fmt"
	"os"
	"syscall"
)

func token(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Unavailable, fmt.Errorf("stat %s for file identity: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Unavailable, nil
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), uint64(stat.Ino)), nil
}
