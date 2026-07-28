//go:build unix

package claude

import (
	"os"
	"testing"
)

// The guarantee is that a warm listing opens no transcript whose stat identity
// is unchanged, so every transcript is made unreadable before the warm run.
// Only a listing served entirely from retained entries can still succeed.
func TestAWarmListingOpensNoTranscript(t *testing.T) {
	fixture := newCacheFixture(t)
	cold := fixture.encoded()
	unreadable := []string{
		fixture.transcript(),
		fixture.subagent(),
		fixture.sidecar(),
	}
	for _, path := range unreadable {
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, path := range unreadable {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Error(err)
			}
		}
	})
	if warm := fixture.encoded(); warm != cold {
		t.Fatalf("warm listing differs\ncold: %s\nwarm: %s", cold, warm)
	}
}
