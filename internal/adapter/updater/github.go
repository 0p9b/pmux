package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/0p9b/pmux/internal/domain/update"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const defaultMaxDownload = int64(512 << 20)

// GitHubSource resolves and downloads GitHub release assets. It performs no
// request until Resolve or Download is explicitly called.
type GitHubSource struct {
	Client      *http.Client
	SelfRepo    string
	ProxyRepo   string
	Target      Target
	MaxDownload int64
}

func NewGitHubSource(client *http.Client, target Target) *GitHubSource {
	if client == nil {
		client = http.DefaultClient
	}
	if target.GOOS == "" || target.Arch == "" {
		target = NativeTarget()
	}
	return &GitHubSource{
		Client: client, SelfRepo: "0p9b/pmux", ProxyRepo: "router-for-me/CLIProxyAPI",
		Target: target, MaxDownload: defaultMaxDownload,
	}
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (s *GitHubSource) Resolve(ctx context.Context, component update.Component, version string) (Release, error) {
	repo, executable, prefix, err := s.component(component)
	if err != nil {
		return Release{}, err
	}
	endpoint := "https://api.github.com/repos/" + repo + "/releases/latest"
	if version != "" {
		tag := version
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		endpoint = "https://api.github.com/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, pmuxerr.Wrap(err, pmuxerr.InstallReleaseLookupFailed, pmuxerr.Internal, "Could not construct the release request.")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return Release{}, pmuxerr.Wrap(err, pmuxerr.InstallReleaseLookupFailed, pmuxerr.Environment, "Could not fetch release metadata.")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Release{}, &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.Upstream, Message: "Could not fetch release metadata.", Evidence: []string{fmt.Sprintf("GitHub returned HTTP %d", resp.StatusCode)}}
	}
	var payload githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return Release{}, pmuxerr.Wrap(err, pmuxerr.InstallReleaseLookupFailed, pmuxerr.Upstream, "GitHub returned invalid release metadata.")
	}

	ext := ".tar.gz"
	if s.Target.GOOS == "windows" {
		ext = ".zip"
	}
	arch := s.Target.Arch
	if component == update.Proxy && arch == "arm64" {
		// CLIProxyAPI release assets use aarch64, not arm64.
		arch = "aarch64"
	}
	needle := "_" + s.Target.GOOS + "_" + arch + ext
	var archiveName, archiveURL, sumsURL string
	for _, asset := range payload.Assets {
		switch {
		case asset.Name == "checksums.txt":
			sumsURL = asset.URL
		case strings.HasPrefix(strings.ToLower(asset.Name), strings.ToLower(prefix)) && strings.HasSuffix(asset.Name, needle):
			if archiveURL != "" {
				return Release{}, &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.Upstream, Message: "Release metadata contains multiple matching platform archives."}
			}
			archiveName, archiveURL = asset.Name, asset.URL
		}
	}
	if archiveURL == "" || sumsURL == "" {
		return Release{}, &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.Upstream, Message: "Release does not contain the required platform archive and checksums.txt.", Evidence: []string{s.Target.GOOS + "/" + s.Target.Arch}}
	}
	return Release{Component: component, Version: strings.TrimPrefix(payload.TagName, "v"), ArchiveName: archiveName, ArchiveURL: archiveURL, ChecksumsURL: sumsURL, ExecutableName: executable}, nil
}

func (s *GitHubSource) component(component update.Component) (repo, executable, prefix string, err error) {
	switch component {
	case update.Self:
		executable = "pmux"
		if s.Target.GOOS == "windows" {
			executable += ".exe"
		}
		return s.SelfRepo, executable, "pmux_", nil
	case update.Proxy:
		executable = "cli-proxy-api"
		if s.Target.GOOS == "windows" {
			executable += ".exe"
		}
		return s.ProxyRepo, executable, "CLIProxyAPI_", nil
	default:
		return "", "", "", &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.User, Message: "Unknown update component."}
	}
}

func (s *GitHubSource) Download(ctx context.Context, sourceURL, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Internal, "Could not construct the download request.")
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "Asset download failed.")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return &pmuxerr.Error{Code: pmuxerr.InstallDownloadFailed, Class: pmuxerr.Upstream, Message: "Asset download failed.", Evidence: []string{fmt.Sprintf("server returned HTTP %d", resp.StatusCode)}}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "Could not create private download staging.")
	}
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "Could not create the staged download.")
	}
	limit := s.MaxDownload
	if limit <= 0 {
		limit = defaultMaxDownload
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written > limit {
		_ = os.Remove(destination)
		if written > limit {
			copyErr = fmt.Errorf("asset exceeds %d bytes", limit)
		}
		if copyErr == nil {
			copyErr = syncErr
		}
		if copyErr == nil {
			copyErr = closeErr
		}
		return pmuxerr.Wrap(copyErr, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "Asset download could not be stored safely.")
	}
	return nil
}
