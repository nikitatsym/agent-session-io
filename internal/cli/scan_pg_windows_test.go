//go:build pgintegration && windows

package cli

import (
	"os/exec"
	"os/user"
	"testing"
)

// unreadable takes a container away from the process without removing it, so a
// listing served from the cache is the only way a command can still see it.
func unreadable(t *testing.T, path string) {
	t.Helper()
	// Windows chmod only flips the read-only attribute; a deny ACE on read-data
	// alone revokes reads and still leaves the attributes mode 000 leaves.
	account := currentAccount(t)
	if output, err := icacls(path, "/deny", account+":(RD)"); err != nil {
		t.Fatalf("icacls deny %s: %v\n%s", path, err, output)
	}
	t.Cleanup(func() {
		if output, err := icacls(path, "/remove:d", account); err != nil {
			t.Errorf("icacls remove %s: %v\n%s", path, err, output)
		}
	})
}

// permissionErrorText is how a refused read renders inside a command error.
func permissionErrorText() string {
	return "Access is denied"
}

func currentAccount(t *testing.T) string {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return account.Username
}

func icacls(path string, arguments ...string) (string, error) {
	output, err := exec.Command(
		"icacls", append([]string{path}, arguments...)...,
	).CombinedOutput()
	return string(output), err
}
