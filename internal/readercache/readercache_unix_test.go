//go:build unix

package readercache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A read-only directory is only a directory a Unix process cannot write.
func TestAnUnwritableDirectoryReportsAndKeepsWorking(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	store := newTestStore(t, filepath.Join(parent, "cache"))
	listingCache(t, store).Retain("occurrence-a", "stamp-1", testRef("a", "x"))
	store.Flush()
	diagnostics := store.Diagnostics()
	if len(diagnostics) != 1 ||
		!strings.Contains(diagnostics[0].String(), "could not write") {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}
