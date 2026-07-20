package installer

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"strings"

	domain "github.com/0p9b/pmux/internal/domain/install"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	ManagedDefaultVersion = "7.2.92"
	defaultReleaseBaseURL = "https://github.com/router-for-me/CLIProxyAPI/releases/download"
	checksumsName         = "checksums.txt"
)

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type Options struct {
	Target         domain.Target
	DataRoot       string
	HTTPClient     *http.Client
	ReleaseBaseURL string
	RestoreService func(context.Context, ServiceCheckpoint) error
}

type Adapter struct {
	target     domain.Target
	dataRoot   string
	httpClient *http.Client
	baseURL    string
	verifiedMu sync.Mutex
	verified   map[string][32]byte
	recoveryMu   sync.Mutex
	recoveryHeld bool
	restoreService func(context.Context, ServiceCheckpoint) error

	previousCurrent string
	activated       bool
	beforeActivate  func() error
	afterActivate   func() error
}

var _ domain.Installer = (*Adapter)(nil)

func New(options Options) (*Adapter, error) {
	target := options.Target
	if target.OS == "" {
		target.OS = runtime.GOOS
	}
	if target.Arch == "" {
		target.Arch = runtime.GOARCH
	}
	if _, err := AssetName(ManagedDefaultVersion, target); err != nil {
		return nil, err
	}
	if options.DataRoot == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.User, "installer data root is required")
	}
	root, err := filepath.Abs(options.DataRoot)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not resolve installer data root")
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	baseURL := strings.TrimRight(options.ReleaseBaseURL, "/")
	if baseURL == "" {
		baseURL = defaultReleaseBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, pmuxerr.New(pmuxerr.InstallReleaseLookupFailed, pmuxerr.User, "release base URL is invalid")
	}
	return &Adapter{
		target: target, dataRoot: root, httpClient: client, baseURL: baseURL,
		verified: make(map[string][32]byte), restoreService: options.RestoreService,
	}, nil
}

func AssetName(version string, target domain.Target) (string, error) {
	if !versionPattern.MatchString(version) {
		return "", pmuxerr.New(pmuxerr.InstallReleaseLookupFailed, pmuxerr.User, "CLIProxyAPI version must be semantic versioning without a leading v")
	}
	var extension string
	switch target.OS {
	case "linux", "darwin":
		extension = "tar.gz"
	case "windows":
		extension = "zip"
	default:
		return "", unsupportedTarget(target)
	}
	switch target.Arch {
	case "amd64", "arm64":
	default:
		return "", unsupportedTarget(target)
	}
	if target.OS == "windows" && target.Arch != "amd64" {
		return "", unsupportedTarget(target)
	}
	return fmt.Sprintf("CLIProxyAPI_%s_%s_%s.%s", version, target.OS, releaseArchToken(target.Arch), extension), nil
}

func unsupportedTarget(target domain.Target) error {
	return &pmuxerr.Error{
		Code:        pmuxerr.InstallUnsupportedTarget,
		Class:       pmuxerr.Environment,
		Message:     fmt.Sprintf("PMux does not support %s/%s", target.OS, target.Arch),
		Explanation: "No managed CLIProxyAPI release asset exists for this PMux v1 target.",
	}
}

func (a *Adapter) Resolve(ctx context.Context, channel string) (domain.Release, error) {
	if err := ctx.Err(); err != nil {
		return domain.Release{}, canceled(err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(channel), "v")
	switch version {
	case "", "managed", "default", "stable", "latest":
		version = ManagedDefaultVersion
	}
	asset, err := AssetName(version, a.target)
	if err != nil {
		return domain.Release{}, err
	}
	tagURL := a.baseURL + "/v" + version
	return domain.Release{
		Version:      version,
		AssetName:    asset,
		AssetURL:     tagURL + "/" + url.PathEscape(asset),
		ChecksumsURL: tagURL + "/" + checksumsName,
	}, nil
}

func (a *Adapter) Download(ctx context.Context, release domain.Release, destination string) (domain.DownloadedAsset, error) {
	expected, err := AssetName(release.Version, a.target)
	if err != nil {
		return domain.DownloadedAsset{}, err
	}
	if release.AssetName != expected {
		return domain.DownloadedAsset{}, pmuxerr.New(pmuxerr.InstallDownloadFailed, pmuxerr.Upstream, "release asset name does not match the selected target")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not create download staging directory")
	}
	finalPath := filepath.Join(destination, release.AssetName)
	if _, err := os.Lstat(finalPath); err == nil {
		return domain.DownloadedAsset{}, pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "download destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not inspect download destination")
	}
	partialPath := finalPath + ".partial"
	if err := a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageDownloading
		record.Target = a.target
		record.Version = release.Version
		record.AssetName = release.AssetName
		record.AssetPath = finalPath
		record.PartialPath = partialPath
	}); err != nil {
		return domain.DownloadedAsset{}, err
	}
	partial, err := os.OpenFile(partialPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not create download staging file")
	}
	committed := false
	defer func() {
		_ = partial.Close()
		if !committed {
			_ = os.Remove(partialPath)
		}
	}()
	if err := partial.Chmod(0o600); err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not protect download staging file")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.AssetURL, nil)
	if err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Internal, "could not build release download request")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return domain.DownloadedAsset{}, canceled(ctx.Err())
		}
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "CLIProxyAPI asset download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return domain.DownloadedAsset{}, pmuxerr.New(pmuxerr.InstallDownloadFailed, pmuxerr.Upstream, fmt.Sprintf("CLIProxyAPI asset download failed with HTTP %d", resp.StatusCode))
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(partial, hash), &contextReader{ctx: ctx, r: resp.Body}); err != nil {
		if ctx.Err() != nil {
			return domain.DownloadedAsset{}, canceled(ctx.Err())
		}
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "CLIProxyAPI asset download failed")
	}
	if err := partial.Sync(); err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not persist downloaded archive")
	}
	if err := partial.Close(); err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not close downloaded archive")
	}
	if err := os.Rename(partialPath, finalPath); err != nil {
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not commit downloaded archive")
	}
	if err := syncDir(destination); err != nil {
		_ = os.Remove(finalPath)
		return domain.DownloadedAsset{}, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "could not persist downloaded archive staging")
	}
	committed = true
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if err := a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageDownloaded
		record.AssetSHA256 = digestHex(digest)
		record.PartialPath = ""
	}); err != nil {
		_ = os.Remove(finalPath)
		return domain.DownloadedAsset{}, err
	}
	return domain.DownloadedAsset{Path: finalPath, Name: release.AssetName, SHA256: digest}, nil
}

func (a *Adapter) DownloadChecksums(ctx context.Context, release domain.Release) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.ChecksumsURL, nil)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Internal, "could not build checksum request")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceled(ctx.Err())
		}
		return nil, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "checksums.txt download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, pmuxerr.New(pmuxerr.InstallDownloadFailed, pmuxerr.Upstream, fmt.Sprintf("checksums.txt download failed with HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: resp.Body}, 4<<20))
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceled(ctx.Err())
		}
		return nil, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "checksums.txt download failed")
	}
	return body, nil
}

func ParseChecksums(body []byte) (map[string][32]byte, error) {
	entries := make(map[string][32]byte)
	for lineNumber, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("checksums.txt line %d is malformed", lineNumber+1))
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) != name || name == "." || name == ".." {
			return nil, pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("checksums.txt line %d has an unsafe asset name", lineNumber+1))
		}
		digestBytes, err := hex.DecodeString(fields[0])
		if err != nil || len(digestBytes) != sha256.Size {
			return nil, pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("checksums.txt line %d has an invalid SHA-256", lineNumber+1))
		}
		if _, exists := entries[name]; exists {
			return nil, pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("checksums.txt contains duplicate entries for %s", name))
		}
		var digest [32]byte
		copy(digest[:], digestBytes)
		entries[name] = digest
	}
	return entries, nil
}

func (a *Adapter) VerifyArchive(ctx context.Context, asset domain.DownloadedAsset, checksums []byte) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	entries, err := ParseChecksums(checksums)
	if err != nil {
		return err
	}
	expected, ok := entries[asset.Name]
	if !ok {
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("checksum missing for %s; refusing to extract", asset.Name))
	}
	file, err := os.Open(asset.Path)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not read downloaded archive for verification")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, r: file}); err != nil {
		if ctx.Err() != nil {
			return canceled(ctx.Err())
		}
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not hash downloaded archive")
	}
	actual := hash.Sum(nil)
	if subtle.ConstantTimeCompare(expected[:], actual) != 1 {
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("checksum mismatch for %s; refusing to extract", asset.Name))
	}
	var verifiedDigest [32]byte
	copy(verifiedDigest[:], actual)
	absolutePath, err := filepath.Abs(asset.Path)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not resolve verified archive path")
	}
	a.verifiedMu.Lock()
	a.verified[absolutePath] = verifiedDigest
	a.verifiedMu.Unlock()
	if err := a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageChecksumVerified
		record.AssetName = asset.Name
		record.AssetPath = absolutePath
		record.AssetSHA256 = digestHex(verifiedDigest)
		record.ExpectedSHA256 = digestHex(expected)
	}); err != nil {
		a.verifiedMu.Lock()
		delete(a.verified, absolutePath)
		a.verifiedMu.Unlock()
		return err
	}
	return nil
}

func (a *Adapter) requireVerifiedArchive(ctx context.Context, archivePath string) error {
	absolutePath, err := filepath.Abs(archivePath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not resolve archive path before extraction")
	}
	a.verifiedMu.Lock()
	expected, ok := a.verified[absolutePath]
	a.verifiedMu.Unlock()
	if !ok {
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive has not passed checksums.txt verification; refusing to extract")
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not reopen verified archive before extraction")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, r: file}); err != nil {
		if ctx.Err() != nil {
			return canceled(ctx.Err())
		}
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not revalidate archive before extraction")
	}
	if subtle.ConstantTimeCompare(expected[:], hash.Sum(nil)) != 1 {
		a.verifiedMu.Lock()
		delete(a.verified, absolutePath)
		a.verifiedMu.Unlock()
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive changed after checksum verification; refusing to extract")
	}
	return nil
}

func (a *Adapter) Extract(ctx context.Context, asset domain.DownloadedAsset, destination string) (domain.ExtractedBinary, error) {
	version, archiveTarget, format, err := parseAssetName(asset.Name)
	if err != nil {
		return domain.ExtractedBinary{}, err
	}
	if archiveTarget != a.target {
		return domain.ExtractedBinary{}, pmuxerr.New(pmuxerr.InstallUnsupportedTarget, pmuxerr.Upstream, "downloaded archive target does not match the selected target")
	}
	if err := a.requireVerifiedArchive(ctx, asset.Path); err != nil {
		return domain.ExtractedBinary{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return domain.ExtractedBinary{}, pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not create extraction staging directory")
	}
	stage := filepath.Join(destination, ".extract-"+version)
	if err := a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageExtracting
		record.Version = version
		record.ExtractStage = stage
	}); err != nil {
		return domain.ExtractedBinary{}, err
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return domain.ExtractedBinary{}, pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not create extraction staging directory")
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	executable := "cli-proxy-api"
	if a.target.OS == "windows" {
		executable += ".exe"
	}
	output := filepath.Join(stage, executable)
	switch format {
	case "tar.gz":
		err = extractTarGZ(ctx, asset.Path, executable, output)
	case "zip":
		err = extractZIP(ctx, asset.Path, executable, output)
	default:
		err = pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "unsupported release archive format")
	}
	if err != nil {
		return domain.ExtractedBinary{}, err
	}
	if err := os.Chmod(output, 0o700); err != nil {
		return domain.ExtractedBinary{}, pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not protect extracted executable")
	}
	if err := a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageExtracted
		record.ExtractedPath = output
	}); err != nil {
		return domain.ExtractedBinary{}, err
	}
	keep = true
	return domain.ExtractedBinary{Path: output, Version: version}, nil
}

func parseAssetName(name string) (string, domain.Target, string, error) {
	if filepath.Base(name) != name {
		return "", domain.Target{}, "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "release asset name is unsafe")
	}
	prefix := "CLIProxyAPI_"
	if !strings.HasPrefix(name, prefix) {
		return "", domain.Target{}, "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "release asset name is not recognized")
	}
	rest := strings.TrimPrefix(name, prefix)
	var format string
	switch {
	case strings.HasSuffix(rest, ".tar.gz"):
		format = "tar.gz"
		rest = strings.TrimSuffix(rest, ".tar.gz")
	case strings.HasSuffix(rest, ".zip"):
		format = "zip"
		rest = strings.TrimSuffix(rest, ".zip")
	default:
		return "", domain.Target{}, "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "release asset archive format is not recognized")
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 3 || !versionPattern.MatchString(parts[0]) {
		return "", domain.Target{}, "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "release asset name is not recognized")
	}
	target := domain.Target{OS: parts[1], Arch: parseReleaseArchToken(parts[2])}
	expected, err := AssetName(parts[0], target)
	if err != nil || expected != name {
		return "", domain.Target{}, "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "release asset name is not a supported exact target asset")
	}
	return parts[0], target, format, nil
}

// releaseArchToken maps Go/runtime arch names onto CLIProxyAPI release tokens.
// Upstream publishes arm64 assets as aarch64.
func releaseArchToken(arch string) string {
	if arch == "arm64" {
		return "aarch64"
	}
	return arch
}

func parseReleaseArchToken(token string) string {
	if token == "aarch64" {
		return "arm64"
	}
	return token
}

func extractTarGZ(ctx context.Context, archivePath, executable, output string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not open verified tar archive")
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "verified archive is not valid gzip")
	}
	defer compressed.Close()
	reader := tar.NewReader(&contextReader{ctx: ctx, r: compressed})
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return canceled(ctx.Err())
			}
			return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "verified tar archive is malformed")
		}
		clean, err := safeArchivePath(header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("archive entry %q is a link or special file; refusing to extract", header.Name))
		}
		if filepath.Base(clean) != executable {
			continue
		}
		if found {
			return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive contains more than one CLIProxyAPI executable")
		}
		if header.Size < 1 {
			return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive contains an empty CLIProxyAPI executable")
		}
		if err := writeExtracted(ctx, output, io.LimitReader(reader, header.Size), header.Size); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive does not contain the expected CLIProxyAPI executable")
	}
	return nil
}

func extractZIP(ctx context.Context, archivePath, executable, output string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "verified archive is not valid zip")
	}
	defer archive.Close()
	found := false
	for _, entry := range archive.File {
		clean, err := safeArchivePath(entry.Name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		if mode.IsDir() {
			continue
		}
		if !mode.IsRegular() {
			return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("archive entry %q is a link or special file; refusing to extract", entry.Name))
		}
		if filepath.Base(clean) != executable {
			continue
		}
		if found {
			return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive contains more than one CLIProxyAPI executable")
		}
		if entry.UncompressedSize64 < 1 || entry.UncompressedSize64 > uint64(^uint64(0)>>1) {
			return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive contains an invalid CLIProxyAPI executable size")
		}
		input, err := entry.Open()
		if err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "could not open CLIProxyAPI executable in zip")
		}
		err = writeExtracted(ctx, output, input, int64(entry.UncompressedSize64))
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return pmuxerr.Wrap(closeErr, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "could not finish reading CLIProxyAPI executable from zip")
		}
		found = true
	}
	if !found {
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "archive does not contain the expected CLIProxyAPI executable")
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") {
		return "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("archive entry %q has an unsafe path", name))
	}
	clean := path.Clean(name)
	windowsVolume := len(name) >= 2 && ((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) && name[1] == ':'
	if clean == "." || path.IsAbs(clean) || windowsVolume || filepath.VolumeName(name) != "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, fmt.Sprintf("archive entry %q escapes extraction staging", name))
	}
	return clean, nil
}

func writeExtracted(ctx context.Context, output string, source io.Reader, size int64) error {
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not create extracted executable")
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(output)
		}
	}()
	written, err := io.Copy(file, &contextReader{ctx: ctx, r: source})
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx.Err())
		}
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "could not extract CLIProxyAPI executable")
	}
	if written != size {
		return pmuxerr.New(pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "CLIProxyAPI executable size did not match archive metadata")
	}
	if err := file.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not persist extracted executable")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "could not close extracted executable")
	}
	ok = true
	return nil
}

func (a *Adapter) VerifyExecutable(ctx context.Context, binary domain.ExtractedBinary, target domain.Target) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	file, err := os.Open(binary.Path)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallUnsupportedTarget, pmuxerr.Environment, "could not open extracted CLIProxyAPI executable")
	}
	var magic [4]byte
	_, readErr := io.ReadFull(file, magic[:])
	_ = file.Close()
	if readErr != nil {
		return pmuxerr.Wrap(readErr, pmuxerr.InstallUnsupportedTarget, pmuxerr.Upstream, "extracted CLIProxyAPI executable is too short")
	}
	wrong := func(reason string) error {
		return &pmuxerr.Error{Code: pmuxerr.InstallUnsupportedTarget, Class: pmuxerr.Upstream, Message: "extracted CLIProxyAPI executable does not match the selected target", Evidence: []string{reason, "target: " + target.OS + "/" + target.Arch}}
	}
	switch target.OS {
	case "linux":
		if string(magic[:]) != "\x7fELF" {
			return wrong("expected ELF executable")
		}
		parsed, err := elf.Open(binary.Path)
		if err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallUnsupportedTarget, pmuxerr.Upstream, "extracted ELF executable is malformed")
		}
		defer parsed.Close()
		expected := elf.EM_X86_64
		if target.Arch == "arm64" {
			expected = elf.EM_AARCH64
		} else if target.Arch != "amd64" {
			return unsupportedTarget(target)
		}
		if parsed.Machine != expected {
			return wrong("ELF architecture: " + parsed.Machine.String())
		}
	case "darwin":
		parsed, err := macho.Open(binary.Path)
		if err != nil {
			return wrong("expected valid Mach-O executable")
		}
		defer parsed.Close()
		expected := macho.CpuAmd64
		if target.Arch == "arm64" {
			expected = macho.CpuArm64
		} else if target.Arch != "amd64" {
			return unsupportedTarget(target)
		}
		if parsed.Cpu != expected {
			return wrong("Mach-O architecture: " + parsed.Cpu.String())
		}
	case "windows":
		if magic[0] != 'M' || magic[1] != 'Z' {
			return wrong("expected PE executable")
		}
		parsed, err := pe.Open(binary.Path)
		if err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallUnsupportedTarget, pmuxerr.Upstream, "extracted PE executable is malformed")
		}
		defer parsed.Close()
		expected := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
		if target.Arch == "arm64" {
			expected = pe.IMAGE_FILE_MACHINE_ARM64
		} else if target.Arch != "amd64" {
			return unsupportedTarget(target)
		}
		if parsed.Machine != expected {
			return wrong(fmt.Sprintf("PE architecture: 0x%04x", parsed.Machine))
		}
	default:
		return unsupportedTarget(target)
	}
	return nil
}

func (a *Adapter) Install(ctx context.Context, binary domain.ExtractedBinary) error {
	if !versionPattern.MatchString(binary.Version) {
		return pmuxerr.New(pmuxerr.InstallUnsupportedTarget, pmuxerr.User, "extracted CLIProxyAPI version is invalid")
	}
	if err := a.VerifyExecutable(ctx, binary, a.target); err != nil {
		return err
	}
	root := filepath.Join(a.dataRoot, "cli-proxy-api")
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not create managed version directory")
	}
	executable := "cli-proxy-api"
	if a.target.OS == "windows" {
		executable += ".exe"
	}
	versionDir := filepath.Join(versions, binary.Version)
	installedPath := filepath.Join(versionDir, executable)
	createdVersion := false
	if _, err := os.Lstat(versionDir); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(versions, ".install-"+binary.Version+"-")
		if err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not create version installation staging")
		}
		stageKept := false
		defer func() {
			if !stageKept {
				_ = os.RemoveAll(stage)
			}
		}()
		if err := copyFileContext(ctx, binary.Path, filepath.Join(stage, executable), 0o700); err != nil {
			return err
		}
		if err := syncDir(stage); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not persist version installation staging")
		}
		if err := os.Rename(stage, versionDir); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not commit managed CLIProxyAPI version")
		}
		stageKept = true
		createdVersion = true
		if err := syncDir(versions); err != nil {
			_ = os.RemoveAll(versionDir)
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not persist managed CLIProxyAPI version")
		}
	} else if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not inspect managed CLIProxyAPI version")
	} else {
		same, err := filesEqual(binary.Path, installedPath)
		if err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not verify immutable managed CLIProxyAPI version")
		}
		if !same {
			return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "managed CLIProxyAPI version already exists with different bytes")
		}
	}
	cleanupVersion := func() {
		if createdVersion {
			_ = os.RemoveAll(versionDir)
		}
	}
	current := filepath.Join(root, "current")
	previous, exists, err := readCurrentPointer(current)
	if err != nil {
		cleanupVersion()
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not read current CLIProxyAPI pointer")
	}
	if a.beforeActivate != nil {
		if err := a.beforeActivate(); err != nil {
			cleanupVersion()
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Internal, "managed CLIProxyAPI activation failed")
		}
	}
	if err := writeCurrentPointer(root, current, versionDir); err != nil {
		cleanupVersion()
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not activate managed CLIProxyAPI version")
	}
	a.previousCurrent = previous
	if !exists {
		a.previousCurrent = ""
	}
	a.activated = true
	if a.afterActivate != nil {
		if err := a.afterActivate(); err != nil {
			rollbackErr := a.Rollback(context.Background())
			cleanupVersion()
			if rollbackErr != nil {
				return &pmuxerr.Error{Code: pmuxerr.InstallRollbackAttempted, Class: pmuxerr.Internal, Message: "managed CLIProxyAPI activation failed and rollback also failed", Cause: errors.Join(err, rollbackErr)}
			}
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Internal, "managed CLIProxyAPI activation failed; prior current version was restored")
		}
	}
	return nil
}

func (a *Adapter) Rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	if !a.activated {
		return nil
	}
	root := filepath.Join(a.dataRoot, "cli-proxy-api")
	current := filepath.Join(root, "current")
	var err error
	if a.previousCurrent == "" {
		err = removeCurrentPointer(current)
	} else {
		err = writeCurrentPointer(root, current, a.previousCurrent)
	}
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not restore prior CLIProxyAPI current pointer")
	}
	a.activated = false
	return nil
}

func copyFileContext(ctx context.Context, source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not open extracted executable for installation")
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not create managed executable")
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, &contextReader{ctx: ctx, r: input}); err != nil {
		if ctx.Err() != nil {
			return canceled(ctx.Err())
		}
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not install managed executable")
	}
	if err := output.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not persist managed executable")
	}
	if err := output.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not close managed executable")
	}
	ok = true
	return nil
}

func filesEqual(left, right string) (bool, error) {
	leftHash, err := hashFile(left)
	if err != nil {
		return false, err
	}
	rightHash, err := hashFile(right)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1, nil
}

func hashFile(path string) ([32]byte, error) {
	var result [32]byte
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(buffer)
	}
}

func canceled(err error) error {
	return pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "operation canceled; partial staging was removed")
}
