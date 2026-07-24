package updater

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/Masterminds/semver/v3"
	selfupdate "github.com/creativeprojects/go-selfupdate"
)

const (
	githubBaseURL  = "https://github.com"
	repositorySlug = "nikitatsym/agent-session-io"
)

// Result describes the outcome of an update attempt.
type Result struct {
	PreviousVersion string
	CurrentVersion  string
	Updated         bool
}

// Service updates a release binary from the project's GitHub releases.
type Service struct {
	backend        releaseBackend
	executablePath func() (string, error)
}

type release struct {
	version string
	native  *selfupdate.Release
}

type releaseBackend interface {
	Latest(context.Context) (release, bool, error)
	Apply(context.Context, release, string) error
}

type selfUpdateBackend struct {
	updater    *selfupdate.Updater
	repository selfupdate.Repository
}

// New creates the production update service.
func New() (*Service, error) {
	source, err := newGitHubReleaseSource(
		http.DefaultClient,
		githubBaseURL,
		runtime.GOOS,
		runtime.GOARCH,
	)
	if err != nil {
		return nil, fmt.Errorf("configure GitHub release source: %w", err)
	}
	nativeUpdater, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
		Filters:   []string{`^sessionio_`},
	})
	if err != nil {
		return nil, fmt.Errorf("configure release updater: %w", err)
	}
	return &Service{
		backend: &selfUpdateBackend{
			updater:    nativeUpdater,
			repository: selfupdate.ParseSlug(repositorySlug),
		},
		executablePath: selfupdate.ExecutablePath,
	}, nil
}

// Update replaces the current executable when a newer release exists.
func (service *Service) Update(ctx context.Context, current string) (Result, error) {
	currentVersion, err := parseCurrentVersion(current)
	if err != nil {
		return Result{}, err
	}
	latest, found, err := service.backend.Latest(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("find latest release: %w", err)
	}
	if !found {
		return Result{}, fmt.Errorf("no release asset matches this operating system and architecture")
	}
	latestVersion, err := semver.NewVersion(latest.version)
	if err != nil {
		return Result{}, fmt.Errorf("parse latest release version %q: %w", latest.version, err)
	}

	result := Result{
		PreviousVersion: currentVersion.String(),
		CurrentVersion:  latestVersion.String(),
	}
	if !latestVersion.GreaterThan(currentVersion) {
		result.CurrentVersion = currentVersion.String()
		return result, nil
	}

	executablePath, err := service.executablePath()
	if err != nil {
		return Result{}, fmt.Errorf("locate current executable: %w", err)
	}
	if err := service.backend.Apply(ctx, latest, executablePath); err != nil {
		return Result{}, fmt.Errorf("replace current executable: %w", err)
	}
	result.Updated = true
	return result, nil
}

func parseCurrentVersion(value string) (*semver.Version, error) {
	if value == "" || value == "dev" || value == "(devel)" {
		return nil, fmt.Errorf("cannot update a development build; install a tagged release first")
	}
	version, err := semver.NewVersion(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("parse current version %q: %w", value, err)
	}
	return version, nil
}

func (backend *selfUpdateBackend) Latest(ctx context.Context) (release, bool, error) {
	nativeRelease, found, err := backend.updater.DetectLatest(ctx, backend.repository)
	if err != nil {
		return release{}, false, err
	}
	if !found {
		return release{}, false, nil
	}
	return release{
		version: nativeRelease.Version(),
		native:  nativeRelease,
	}, true, nil
}

func (backend *selfUpdateBackend) Apply(
	ctx context.Context,
	target release,
	executablePath string,
) error {
	return backend.updater.UpdateTo(ctx, target.native, executablePath)
}
