package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nikitatsym/agent-session-io/internal/buildinfo"
	"github.com/nikitatsym/agent-session-io/internal/completion"
	"github.com/nikitatsym/agent-session-io/internal/updater"
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

func TestCompletionInstallConnectsDetectedShell(t *testing.T) {
	homeDir := t.TempDir()
	root := newRoot(
		buildinfo.Info{Version: "0.1.2"},
		rootOptions{
			completionEnvironment: func() (completion.Environment, error) {
				return completion.Environment{
					HomeDir:   homeDir,
					ConfigDir: filepath.Join(homeDir, ".config"),
					Shell:     "/bin/zsh",
					RuntimeOS: "darwin",
				}, nil
			},
			newUpdater: unusedUpdaterFactory(t),
		},
	)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"completion", "install"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute completion install: %v", err)
	}
	if !strings.Contains(output.String(), "installed zsh completion") {
		t.Fatalf("output = %q, want zsh installation result", output.String())
	}
	if !strings.Contains(output.String(), "restart the shell") {
		t.Fatalf("output = %q, want activation hint", output.String())
	}
}

func TestUpdateReportsInstalledVersion(t *testing.T) {
	service := &fakeUpdateService{
		result: updater.Result{
			PreviousVersion: "0.1.2",
			CurrentVersion:  "0.2.0",
			Updated:         true,
		},
	}
	root := newRoot(
		buildinfo.Info{Version: "0.1.2"},
		rootOptions{
			completionEnvironment: unusedCompletionEnvironment(t),
			newUpdater: func() (updateService, error) {
				return service, nil
			},
		},
	)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"update"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute update: %v", err)
	}
	const want = "updated sessionio from 0.1.2 to 0.2.0\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
	if service.currentVersion != "0.1.2" {
		t.Fatalf("current version = %q, want 0.1.2", service.currentVersion)
	}
}

type fakeUpdateService struct {
	result         updater.Result
	err            error
	currentVersion string
}

func (service *fakeUpdateService) Update(
	_ context.Context,
	currentVersion string,
) (updater.Result, error) {
	service.currentVersion = currentVersion
	return service.result, service.err
}

func unusedUpdaterFactory(t *testing.T) func() (updateService, error) {
	t.Helper()
	return func() (updateService, error) {
		t.Fatal("updater factory was called")
		return nil, nil
	}
}

func unusedCompletionEnvironment(
	t *testing.T,
) func() (completion.Environment, error) {
	t.Helper()
	return func() (completion.Environment, error) {
		t.Fatal("completion environment was requested")
		return completion.Environment{}, nil
	}
}
