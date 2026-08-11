//go:build pgintegration && unix

package cli

import (
	"os"
	"testing"
)

// unreadable takes a container away from the process without removing it, so a
// listing served from the cache is the only way a command can still see it.
func unreadable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Error(err)
		}
	})
}

// permissionErrorText is how a refused read renders inside a command error.
func permissionErrorText() string {
	return "permission denied"
}
