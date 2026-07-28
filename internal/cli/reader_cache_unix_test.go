//go:build unix

package cli

import (
	"os"
	"strings"
	"testing"
)

// A cache directory that cannot be written costs nothing but a diagnostic.
func TestAnUnwritableCacheDirectoryStillLists(t *testing.T) {
	fixture := newListingFixture(t)
	cold, _ := fixture.run("list", "--format", "ndjson")
	if err := os.RemoveAll(fixture.cacheDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.cacheDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(fixture.cacheDir, 0o700); err != nil {
			t.Error(err)
		}
	})
	listed, diagnostic := fixture.run("list", "--format", "ndjson")
	if listed != cold {
		t.Fatalf("an unwritable cache changed the listing\ncold: %s\nafter: %s",
			cold, listed)
	}
	if !strings.Contains(diagnostic, "could not write") {
		t.Fatalf("stderr = %q", diagnostic)
	}
}
