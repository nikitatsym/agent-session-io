package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	selfupdate "github.com/creativeprojects/go-selfupdate"
)

const (
	releaseAssetID  int64 = 1
	checksumAssetID int64 = 2
)

type githubReleaseSource struct {
	client  *http.Client
	baseURL *url.URL
	os      string
	arch    string
}

func newGitHubReleaseSource(
	client *http.Client,
	baseURL string,
	os string,
	arch string,
) (*githubReleaseSource, error) {
	if client == nil {
		return nil, fmt.Errorf("HTTP client is required")
	}
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL %q: %w", baseURL, err)
	}
	if parsedBaseURL.Scheme == "" || parsedBaseURL.Host == "" {
		return nil, fmt.Errorf("base URL %q must include a scheme and host", baseURL)
	}
	if os == "" || arch == "" {
		return nil, fmt.Errorf("operating system and architecture are required")
	}
	return &githubReleaseSource{
		client:  client,
		baseURL: parsedBaseURL,
		os:      os,
		arch:    arch,
	}, nil
}

func (source *githubReleaseSource) ListReleases(
	ctx context.Context,
	repository selfupdate.Repository,
) ([]selfupdate.SourceRelease, error) {
	owner, name, err := repository.GetSlug()
	if err != nil {
		return nil, fmt.Errorf("read repository slug: %w", err)
	}
	tag, err := source.latestTag(ctx, owner, name)
	if err != nil {
		return nil, err
	}

	releaseURL, err := url.JoinPath(
		source.baseURL.String(),
		owner,
		name,
		"releases",
		"tag",
		tag,
	)
	if err != nil {
		return nil, fmt.Errorf("build release URL: %w", err)
	}
	downloadBaseURL, err := url.JoinPath(
		source.baseURL.String(),
		owner,
		name,
		"releases",
		"download",
		tag,
	)
	if err != nil {
		return nil, fmt.Errorf("build release download URL: %w", err)
	}
	assetName := source.assetName()
	assetURL, err := url.JoinPath(downloadBaseURL, assetName)
	if err != nil {
		return nil, fmt.Errorf("build release asset URL: %w", err)
	}
	checksumURL, err := url.JoinPath(downloadBaseURL, "checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("build checksum asset URL: %w", err)
	}

	return []selfupdate.SourceRelease{&selfupdate.HttpRelease{
		ID:      1,
		Name:    tag,
		TagName: tag,
		URL:     releaseURL,
		Assets: []*selfupdate.HttpAsset{
			{
				ID:   releaseAssetID,
				Name: assetName,
				URL:  assetURL,
			},
			{
				ID:   checksumAssetID,
				Name: "checksums.txt",
				URL:  checksumURL,
			},
		},
	}}, nil
}

func (source *githubReleaseSource) DownloadReleaseAsset(
	ctx context.Context,
	release *selfupdate.Release,
	assetID int64,
) (io.ReadCloser, error) {
	if release == nil {
		return nil, selfupdate.ErrInvalidRelease
	}

	var downloadURL string
	switch assetID {
	case release.AssetID:
		downloadURL = release.AssetURL
	case release.ValidationAssetID:
		downloadURL = release.ValidationAssetURL
	default:
		return nil, fmt.Errorf("release asset ID %d: %w", assetID, selfupdate.ErrAssetNotFound)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create release asset request for %s: %w", downloadURL, err)
	}
	response, err := source.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download release asset from %s: %w", downloadURL, err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf(
			"download release asset from %s: HTTP %s",
			downloadURL,
			response.Status,
		)
	}
	return response.Body, nil
}

func (source *githubReleaseSource) latestTag(
	ctx context.Context,
	owner string,
	name string,
) (string, error) {
	latestURL, err := url.JoinPath(
		source.baseURL.String(),
		owner,
		name,
		"releases",
		"latest",
	)
	if err != nil {
		return "", fmt.Errorf("build latest release URL: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, latestURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create latest release request for %s: %w", latestURL, err)
	}
	response, err := source.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("resolve latest release from %s: %w", latestURL, err)
	}
	response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"resolve latest release from %s: HTTP %s",
			latestURL,
			response.Status,
		)
	}
	if response.Request == nil || response.Request.URL == nil {
		return "", fmt.Errorf("resolve latest release from %s: response has no final URL", latestURL)
	}
	if response.Request.URL.Scheme != source.baseURL.Scheme ||
		response.Request.URL.Host != source.baseURL.Host {
		return "", fmt.Errorf(
			"resolve latest release from %s: unexpected redirect host %q",
			latestURL,
			response.Request.URL.Host,
		)
	}

	tagPrefixURL, err := url.JoinPath(
		source.baseURL.String(),
		owner,
		name,
		"releases",
		"tag",
	)
	if err != nil {
		return "", fmt.Errorf("build release tag URL: %w", err)
	}
	parsedTagPrefixURL, err := url.Parse(tagPrefixURL)
	if err != nil {
		return "", fmt.Errorf("parse release tag URL %q: %w", tagPrefixURL, err)
	}
	tagPrefix := strings.TrimSuffix(parsedTagPrefixURL.Path, "/") + "/"
	tag, found := strings.CutPrefix(response.Request.URL.Path, tagPrefix)
	if !found || tag == "" || strings.Contains(tag, "/") {
		return "", fmt.Errorf(
			"resolve latest release from %s: unexpected final URL %s",
			latestURL,
			response.Request.URL.String(),
		)
	}
	return tag, nil
}

func (source *githubReleaseSource) assetName() string {
	extension := ".tar.gz"
	if source.os == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf("sessionio_%s_%s%s", source.os, source.arch, extension)
}

var _ selfupdate.Source = (*githubReleaseSource)(nil)
