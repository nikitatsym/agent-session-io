package buildinfo

import "testing"

func TestCurrentUsesLinkerValues(t *testing.T) {
	previousVersion, previousCommit, previousDate := version, commit, date
	t.Cleanup(func() {
		version, commit, date = previousVersion, previousCommit, previousDate
	})

	version = "0.1.2"
	commit = "0123456789abcdef"
	date = "2026-07-24T12:00:00Z"

	got := Current()
	if got.Version != version {
		t.Fatalf("version = %q, want %q", got.Version, version)
	}
	if got.Commit != commit {
		t.Fatalf("commit = %q, want %q", got.Commit, commit)
	}
	if got.Date != date {
		t.Fatalf("date = %q, want %q", got.Date, date)
	}
}
