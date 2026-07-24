package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	selfupdate "github.com/creativeprojects/go-selfupdate"
)

func TestGitHubReleaseSourceResolvesLatestWithoutAPI(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		requestedPaths = append(requestedPaths, request.URL.Path)
		switch request.URL.Path {
		case "/owner/project/releases/latest":
			http.Redirect(
				writer,
				request,
				"/owner/project/releases/tag/v1.2.3",
				http.StatusFound,
			)
		case "/owner/project/releases/tag/v1.2.3":
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	source, err := newGitHubReleaseSource(
		server.Client(),
		server.URL,
		"linux",
		"arm64",
	)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	releases, err := source.ListReleases(
		context.Background(),
		selfupdate.ParseSlug("owner/project"),
	)
	if err != nil {
		t.Fatalf("list releases: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("release count = %d, want 1", len(releases))
	}
	if releases[0].GetTagName() != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3", releases[0].GetTagName())
	}
	assets := releases[0].GetAssets()
	if len(assets) != 2 {
		t.Fatalf("asset count = %d, want 2", len(assets))
	}
	if assets[0].GetName() != "sessionio_linux_arm64.tar.gz" {
		t.Fatalf("release asset = %q", assets[0].GetName())
	}
	if assets[1].GetName() != "checksums.txt" {
		t.Fatalf("checksum asset = %q", assets[1].GetName())
	}
	if !slices.Equal(requestedPaths, []string{
		"/owner/project/releases/latest",
		"/owner/project/releases/tag/v1.2.3",
	}) {
		t.Fatalf("requested paths = %#v", requestedPaths)
	}
}

func TestGitHubReleaseSourceRejectsLatestWithoutTagRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	source, err := newGitHubReleaseSource(
		server.Client(),
		server.URL,
		"linux",
		"amd64",
	)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	_, err = source.ListReleases(
		context.Background(),
		selfupdate.ParseSlug("owner/project"),
	)
	if err == nil || !strings.Contains(err.Error(), "unexpected final URL") {
		t.Fatalf("error = %v, want unexpected final URL", err)
	}
}

func TestDirectReleaseUpdateVerifiesChecksumAndReplacesExecutable(t *testing.T) {
	const updatedContent = "updated-sessionio"
	archive := releaseArchive(t, updatedContent)
	checksum := fmt.Sprintf(
		"%x  sessionio_linux_amd64.tar.gz\n",
		sha256.Sum256(archive),
	)
	server := releaseServer(t, archive, checksum)
	defer server.Close()

	service := directReleaseService(t, server)
	executablePath := testExecutable(t, service)

	result, err := service.Update(context.Background(), "0.1.0")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !result.Updated || result.CurrentVersion != "1.2.3" {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("read updated executable: %v", err)
	}
	if string(content) != updatedContent {
		t.Fatalf("updated content = %q, want %q", content, updatedContent)
	}
}

func TestDirectReleaseUpdateRejectsInvalidChecksum(t *testing.T) {
	archive := releaseArchive(t, "updated-sessionio")
	server := releaseServer(
		t,
		archive,
		"0000000000000000000000000000000000000000000000000000000000000000"+
			"  sessionio_linux_amd64.tar.gz\n",
	)
	defer server.Close()

	service := directReleaseService(t, server)
	executablePath := testExecutable(t, service)

	_, err := service.Update(context.Background(), "0.1.0")
	if err == nil || !strings.Contains(err.Error(), "sha256 validation failed") {
		t.Fatalf("error = %v, want checksum validation error", err)
	}
	content, readErr := os.ReadFile(executablePath)
	if readErr != nil {
		t.Fatalf("read original executable: %v", readErr)
	}
	if string(content) != "old-sessionio" {
		t.Fatalf("content after failed update = %q", content)
	}
}

func testExecutable(t *testing.T, service *Service) string {
	t.Helper()
	executablePath := filepath.Join(t.TempDir(), "sessionio")
	if err := os.WriteFile(executablePath, []byte("old-sessionio"), 0o755); err != nil {
		t.Fatalf("write old executable: %v", err)
	}
	service.executablePath = func() (string, error) {
		return executablePath, nil
	}
	return executablePath
}

func directReleaseService(t *testing.T, server *httptest.Server) *Service {
	t.Helper()
	source, err := newGitHubReleaseSource(
		server.Client(),
		server.URL,
		"linux",
		"amd64",
	)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	nativeUpdater, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
		Filters:   []string{`^sessionio_`},
		OS:        "linux",
		Arch:      "amd64",
	})
	if err != nil {
		t.Fatalf("create updater: %v", err)
	}
	return &Service{
		backend: &selfUpdateBackend{
			updater:    nativeUpdater,
			repository: selfupdate.ParseSlug("owner/project"),
		},
	}
}

func releaseServer(
	t *testing.T,
	archive []byte,
	checksum string,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/owner/project/releases/latest":
			http.Redirect(
				writer,
				request,
				"/owner/project/releases/tag/v1.2.3",
				http.StatusFound,
			)
		case "/owner/project/releases/tag/v1.2.3":
			writer.WriteHeader(http.StatusOK)
		case "/owner/project/releases/download/v1.2.3/sessionio_linux_amd64.tar.gz":
			if _, err := writer.Write(archive); err != nil {
				t.Errorf("write archive response: %v", err)
			}
		case "/owner/project/releases/download/v1.2.3/checksums.txt":
			if _, err := io.WriteString(writer, checksum); err != nil {
				t.Errorf("write checksum response: %v", err)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
}

func releaseArchive(t *testing.T, executableContent string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte(executableContent)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "sessionio",
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return archive.Bytes()
}
