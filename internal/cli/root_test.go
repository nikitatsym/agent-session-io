package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
)

func TestVersionJSON(t *testing.T) {
	info := buildinfo.Info{
		Version: "0.1.2",
		Commit:  "0123456789abcdef",
		Date:    "2026-07-24T12:00:00Z",
	}
	root := NewRoot(info)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"version", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}

	var got versionRecord
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode version output: %v", err)
	}
	if got.Schema != versionSchema {
		t.Fatalf("schema = %q, want %q", got.Schema, versionSchema)
	}
	if got.Info != info {
		t.Fatalf("info = %#v, want %#v", got.Info, info)
	}
}

func TestVersionTextShortensCommit(t *testing.T) {
	root := NewRoot(buildinfo.Info{
		Version: "0.1.2",
		Commit:  "0123456789abcdef",
		Dirty:   true,
	})
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}

	const want = "sessionio 0.1.2 (0123456789ab) dirty\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}
