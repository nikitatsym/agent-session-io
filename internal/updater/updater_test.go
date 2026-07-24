package updater

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUpdateAppliesNewerRelease(t *testing.T) {
	backend := &fakeBackend{
		latest: release{version: "0.2.0"},
		found:  true,
	}
	service := &Service{
		backend: backend,
		executablePath: func() (string, error) {
			return "/tmp/sessionio", nil
		},
	}

	result, err := service.Update(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !result.Updated {
		t.Fatal("updated = false, want true")
	}
	if result.PreviousVersion != "0.1.0" || result.CurrentVersion != "0.2.0" {
		t.Fatalf("result = %#v", result)
	}
	if backend.appliedPath != "/tmp/sessionio" {
		t.Fatalf("applied path = %q, want /tmp/sessionio", backend.appliedPath)
	}
}

func TestUpdateDoesNotDowngradeNewerBuild(t *testing.T) {
	backend := &fakeBackend{
		latest: release{version: "0.2.0"},
		found:  true,
	}
	service := &Service{
		backend: backend,
		executablePath: func() (string, error) {
			t.Fatal("executable path requested for an up-to-date build")
			return "", nil
		},
	}

	result, err := service.Update(context.Background(), "0.3.0")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if result.Updated {
		t.Fatal("updated = true, want false")
	}
	if result.CurrentVersion != "0.3.0" {
		t.Fatalf("current version = %q, want 0.3.0", result.CurrentVersion)
	}
	if backend.applyCalls != 0 {
		t.Fatalf("apply calls = %d, want 0", backend.applyCalls)
	}
}

func TestUpdateRejectsDevelopmentBuild(t *testing.T) {
	service := &Service{backend: &fakeBackend{}}
	_, err := service.Update(context.Background(), "dev")
	if err == nil || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("error = %v, want development build error", err)
	}
}

func TestUpdatePreservesApplyErrorContext(t *testing.T) {
	backend := &fakeBackend{
		latest:   release{version: "0.2.0"},
		found:    true,
		applyErr: errors.New("permission denied"),
	}
	service := &Service{
		backend: backend,
		executablePath: func() (string, error) {
			return "/opt/sessionio", nil
		},
	}

	_, err := service.Update(context.Background(), "0.1.0")
	if err == nil || !strings.Contains(err.Error(), "replace current executable: permission denied") {
		t.Fatalf("error = %v, want replacement context", err)
	}
}

type fakeBackend struct {
	latest      release
	found       bool
	latestErr   error
	applyErr    error
	applyCalls  int
	appliedPath string
}

func (backend *fakeBackend) Latest(context.Context) (release, bool, error) {
	return backend.latest, backend.found, backend.latestErr
}

func (backend *fakeBackend) Apply(
	_ context.Context,
	_ release,
	executablePath string,
) error {
	backend.applyCalls++
	backend.appliedPath = executablePath
	return backend.applyErr
}
