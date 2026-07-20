package installer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	domain "github.com/0p9b/pmux/internal/domain/install"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestResolveManagedDefaultAssetNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target domain.Target
		name   string
	}{
		{domain.Target{OS: "linux", Arch: "amd64"}, "CLIProxyAPI_7.2.92_linux_amd64.tar.gz"},
		{domain.Target{OS: "linux", Arch: "arm64"}, "CLIProxyAPI_7.2.92_linux_aarch64.tar.gz"},
		{domain.Target{OS: "darwin", Arch: "amd64"}, "CLIProxyAPI_7.2.92_darwin_amd64.tar.gz"},
		{domain.Target{OS: "darwin", Arch: "arm64"}, "CLIProxyAPI_7.2.92_darwin_aarch64.tar.gz"},
		{domain.Target{OS: "windows", Arch: "amd64"}, "CLIProxyAPI_7.2.92_windows_amd64.zip"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.target.OS+"-"+test.target.Arch, func(t *testing.T) {
			t.Parallel()
			adapter := newTestAdapter(t, test.target)
			release, err := adapter.Resolve(context.Background(), "managed")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if release.Version != ManagedDefaultVersion || release.AssetName != test.name {
				t.Fatalf("unexpected release: %#v", release)
			}
			wantPrefix := "https://example.test/releases/v7.2.92/"
			if release.AssetURL != wantPrefix+test.name || release.ChecksumsURL != wantPrefix+"checksums.txt" {
				t.Fatalf("unexpected URLs: %#v", release)
			}
		})
	}
}

func TestAssetNameRejectsNonV1Target(t *testing.T) {
	t.Parallel()
	_, err := AssetName(ManagedDefaultVersion, domain.Target{OS: "windows", Arch: "arm64"})
	assertPMuxCode(t, err, pmuxerr.InstallUnsupportedTarget)
}

func TestDownloadStagesAtomicallyAndRemovesCanceledPartial(t *testing.T) {
	t.Parallel()
	body := []byte("verified archive bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	adapter := newTestAdapterWithClient(t, domain.Target{OS: "linux", Arch: "amd64"}, server.Client())
	release, err := adapter.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	release.AssetURL = server.URL + "/asset"
	downloadDir := t.TempDir()
	asset, err := adapter.Download(context.Background(), release, downloadDir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	read, err := os.ReadFile(asset.Path)
	if err != nil || !bytes.Equal(read, body) {
		t.Fatalf("downloaded bytes = %q, %v", read, err)
	}
	if asset.SHA256 != sha256.Sum256(body) {
		t.Fatal("download digest differs")
	}
	assertNoPartialFiles(t, downloadDir)

	canceledDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = adapter.Download(ctx, release, canceledDir)
	if err == nil {
		t.Fatal("canceled download succeeded")
	}
	assertNoFiles(t, canceledDir)
}

func TestDownloadChecksums(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/checksums.txt" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "abc")
	}))
	defer server.Close()
	adapter := newTestAdapterWithClient(t, domain.Target{OS: "linux", Arch: "amd64"}, server.Client())
	body, err := adapter.DownloadChecksums(context.Background(), domain.Release{ChecksumsURL: server.URL + "/checksums.txt"})
	if err != nil || string(body) != "abc" {
		t.Fatalf("DownloadChecksums = %q, %v", body, err)
	}
}

func TestVerifyArchiveRequiresExactMatchingChecksumBeforeExtraction(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t, domain.Target{OS: "linux", Arch: "amd64"})
	archive := filepath.Join(t.TempDir(), "asset.tar.gz")
	body := []byte("archive")
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	name := "CLIProxyAPI_7.2.92_linux_amd64.tar.gz"
	asset := domain.DownloadedAsset{Path: archive, Name: name, SHA256: sha256.Sum256(body)}

	err := adapter.VerifyArchive(context.Background(), asset, []byte(strings.Repeat("0", 64)+"  other.tar.gz\n"))
	assertPMuxCode(t, err, pmuxerr.InstallIntegrityFailed)

	err = adapter.VerifyArchive(context.Background(), asset, []byte(strings.Repeat("0", 64)+"  "+name+"\n"))
	assertPMuxCode(t, err, pmuxerr.InstallIntegrityFailed)
	blockedDestination := t.TempDir()
	_, extractErr := adapter.Extract(context.Background(), asset, blockedDestination)
	assertPMuxCode(t, extractErr, pmuxerr.InstallIntegrityFailed)
	assertNoFiles(t, blockedDestination)

	digest := sha256.Sum256(body)
	err = adapter.VerifyArchive(context.Background(), asset, []byte(hex.EncodeToString(digest[:])+" *"+name+"\n"))
	if err != nil {
		t.Fatalf("matching checksum rejected: %v", err)
	}
}

func TestExtractRefusesArchiveChangedAfterVerification(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t, domain.Target{OS: "linux", Arch: "amd64"})
	name := "CLIProxyAPI_7.2.92_linux_amd64.tar.gz"
	archivePath := filepath.Join(t.TempDir(), name)
	if err := makeTarGZ(archivePath, []archiveEntry{{name: "cli-proxy-api", body: []byte("original")}}); err != nil {
		t.Fatal(err)
	}
	verifyArchiveForExtract(t, adapter, archivePath, name)
	if err := os.WriteFile(archivePath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	_, err := adapter.Extract(context.Background(), domain.DownloadedAsset{Path: archivePath, Name: name}, destination)
	assertPMuxCode(t, err, pmuxerr.InstallIntegrityFailed)
	assertNoFiles(t, destination)
}

func TestParseChecksumsRejectsDuplicateAndUnsafeEntries(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	for _, body := range []string{
		digest + "  a.zip\n" + digest + "  a.zip\n",
		digest + "  ../a.zip\n",
		"not-a-hash  a.zip\n",
	} {
		if _, err := ParseChecksums([]byte(body)); err == nil {
			t.Fatalf("ParseChecksums(%q) succeeded", body)
		}
	}
}

func TestExtractTarAndZipExpectedExecutable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target domain.Target
		name   string
		entry  string
		make   func(string, []archiveEntry) error
	}{
		{domain.Target{OS: "linux", Arch: "amd64"}, "CLIProxyAPI_7.2.92_linux_amd64.tar.gz", "release/cli-proxy-api", makeTarGZ},
		{domain.Target{OS: "windows", Arch: "amd64"}, "CLIProxyAPI_7.2.92_windows_amd64.zip", "release/cli-proxy-api.exe", makeZIP},
	}
	for _, test := range tests {
		test := test
		t.Run(test.target.OS, func(t *testing.T) {
			t.Parallel()
			adapter := newTestAdapter(t, test.target)
			archivePath := filepath.Join(t.TempDir(), test.name)
			if err := test.make(archivePath, []archiveEntry{{name: "README", body: []byte("ignored")}, {name: test.entry, body: []byte("binary")}}); err != nil {
				t.Fatal(err)
			}
			verifyArchiveForExtract(t, adapter, archivePath, test.name)
			result, err := adapter.Extract(context.Background(), domain.DownloadedAsset{Path: archivePath, Name: test.name}, t.TempDir())
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			body, err := os.ReadFile(result.Path)
			if err != nil || string(body) != "binary" || result.Version != ManagedDefaultVersion {
				t.Fatalf("result = %#v, body %q, err %v", result, body, err)
			}
		})
	}
}

func TestExtractRejectsTraversalAndLinksAndRemovesStaging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		target  domain.Target
		asset   string
		entries []archiveEntry
		make    func(string, []archiveEntry) error
	}{
		{"tar traversal", domain.Target{OS: "linux", Arch: "amd64"}, "CLIProxyAPI_7.2.92_linux_amd64.tar.gz", []archiveEntry{{name: "../cli-proxy-api", body: []byte("bad")}}, makeTarGZ},
		{"tar symlink", domain.Target{OS: "linux", Arch: "amd64"}, "CLIProxyAPI_7.2.92_linux_amd64.tar.gz", []archiveEntry{{name: "link", link: "cli-proxy-api"}, {name: "cli-proxy-api", body: []byte("bad")}}, makeTarGZ},
		{"zip traversal", domain.Target{OS: "windows", Arch: "amd64"}, "CLIProxyAPI_7.2.92_windows_amd64.zip", []archiveEntry{{name: "../cli-proxy-api.exe", body: []byte("bad")}}, makeZIP},
		{"zip symlink", domain.Target{OS: "windows", Arch: "amd64"}, "CLIProxyAPI_7.2.92_windows_amd64.zip", []archiveEntry{{name: "link", link: "cli-proxy-api.exe"}, {name: "cli-proxy-api.exe", body: []byte("bad")}}, makeZIP},
		{"windows drive", domain.Target{OS: "windows", Arch: "amd64"}, "CLIProxyAPI_7.2.92_windows_amd64.zip", []archiveEntry{{name: "C:/cli-proxy-api.exe", body: []byte("bad")}}, makeZIP},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter := newTestAdapter(t, test.target)
			archivePath := filepath.Join(t.TempDir(), test.asset)
			if err := test.make(archivePath, test.entries); err != nil {
				t.Fatal(err)
			}
			verifyArchiveForExtract(t, adapter, archivePath, test.asset)
			destination := t.TempDir()
			_, err := adapter.Extract(context.Background(), domain.DownloadedAsset{Path: archivePath, Name: test.asset}, destination)
			assertPMuxCode(t, err, pmuxerr.InstallIntegrityFailed)
			assertNoFiles(t, destination)
		})
	}
}

func TestExtractCancellationRemovesStaging(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t, domain.Target{OS: "linux", Arch: "amd64"})
	name := "CLIProxyAPI_7.2.92_linux_amd64.tar.gz"
	archivePath := filepath.Join(t.TempDir(), name)
	if err := makeTarGZ(archivePath, []archiveEntry{{name: "cli-proxy-api", body: bytes.Repeat([]byte("x"), 1024)}}); err != nil {
		t.Fatal(err)
	}
	verifyArchiveForExtract(t, adapter, archivePath, name)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	destination := t.TempDir()
	_, err := adapter.Extract(ctx, domain.DownloadedAsset{Path: archivePath, Name: name}, destination)
	if err == nil {
		t.Fatal("canceled extraction succeeded")
	}
	assertNoFiles(t, destination)
}

func TestVerifyExecutableMagicAndArchitecture(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target domain.Target
		body   []byte
	}{
		{"ELF amd64", domain.Target{OS: "linux", Arch: "amd64"}, minimalELF(elf.EM_X86_64)},
		{"ELF arm64", domain.Target{OS: "linux", Arch: "arm64"}, minimalELF(elf.EM_AARCH64)},
		{"Mach-O amd64", domain.Target{OS: "darwin", Arch: "amd64"}, minimalMachO(uint32(macho.CpuAmd64))},
		{"Mach-O arm64", domain.Target{OS: "darwin", Arch: "arm64"}, minimalMachO(uint32(macho.CpuArm64))},
		{"PE amd64", domain.Target{OS: "windows", Arch: "amd64"}, minimalPE(pe.IMAGE_FILE_MACHINE_AMD64)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "binary")
			if err := os.WriteFile(path, test.body, 0o700); err != nil {
				t.Fatal(err)
			}
			adapter := newTestAdapter(t, test.target)
			if err := adapter.VerifyExecutable(context.Background(), domain.ExtractedBinary{Path: path}, test.target); err != nil {
				t.Fatalf("VerifyExecutable: %v", err)
			}
		})
	}
}

func TestVerifyExecutableRejectsWrongMagicAndArchitecture(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, minimalELF(elf.EM_AARCH64), 0o700); err != nil {
		t.Fatal(err)
	}
	adapter := newTestAdapter(t, domain.Target{OS: "linux", Arch: "amd64"})
	assertPMuxCode(t, adapter.VerifyExecutable(context.Background(), domain.ExtractedBinary{Path: path}, domain.Target{OS: "linux", Arch: "amd64"}), pmuxerr.InstallUnsupportedTarget)

	if err := os.WriteFile(path, minimalMachO(uint32(macho.CpuAmd64)), 0o700); err != nil {
		t.Fatal(err)
	}
	assertPMuxCode(t, adapter.VerifyExecutable(context.Background(), domain.ExtractedBinary{Path: path}, domain.Target{OS: "linux", Arch: "amd64"}), pmuxerr.InstallUnsupportedTarget)

	if err := os.WriteFile(path, minimalMachO(uint32(macho.CpuArm64)), 0o700); err != nil {
		t.Fatal(err)
	}
	darwinAdapter := newTestAdapter(t, domain.Target{OS: "darwin", Arch: "amd64"})
	assertPMuxCode(t, darwinAdapter.VerifyExecutable(context.Background(), domain.ExtractedBinary{Path: path}, domain.Target{OS: "darwin", Arch: "amd64"}), pmuxerr.InstallUnsupportedTarget)

	if err := os.WriteFile(path, minimalPE(pe.IMAGE_FILE_MACHINE_ARM64), 0o700); err != nil {
		t.Fatal(err)
	}
	windowsAdapter := newTestAdapter(t, domain.Target{OS: "windows", Arch: "amd64"})
	assertPMuxCode(t, windowsAdapter.VerifyExecutable(context.Background(), domain.ExtractedBinary{Path: path}, domain.Target{OS: "windows", Arch: "amd64"}), pmuxerr.InstallUnsupportedTarget)
}

func TestInstallActivationRollbackAndInjectedFailureRetainsPriorCurrent(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("uses the running test executable as a valid linux/amd64 ELF fixture")
	}
	root := t.TempDir()
	adapter, err := New(Options{Target: domain.Target{OS: "linux", Arch: "amd64"}, DataRoot: root, ReleaseBaseURL: "https://example.test/releases"})
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	first := fixtureBinary(t, testExecutable, "7.2.91")
	if err := adapter.Install(context.Background(), first); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	current := filepath.Join(root, "cli-proxy-api", "current")
	firstCurrent, exists, err := readCurrentPointer(current)
	if err != nil || !exists || filepath.Base(firstCurrent) != "7.2.91" {
		t.Fatalf("first current = %q, %v, %v", firstCurrent, exists, err)
	}

	second := fixtureBinary(t, testExecutable, "7.2.92")
	adapter.afterActivate = func() error { return errors.New("injected post-activation failure") }
	err = adapter.Install(context.Background(), second)
	assertPMuxCode(t, err, pmuxerr.InstallRollbackAttempted)
	afterFailure, exists, readErr := readCurrentPointer(current)
	if readErr != nil || !exists || afterFailure != firstCurrent {
		t.Fatalf("current after failure = %q, %v, %v; want %q", afterFailure, exists, readErr, firstCurrent)
	}
	if _, statErr := os.Stat(filepath.Join(root, "cli-proxy-api", "versions", "7.2.92")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed version was not removed: %v", statErr)
	}
	assertNoPartialFiles(t, filepath.Join(root, "cli-proxy-api", "versions"))

	adapter.afterActivate = nil
	if err := adapter.Install(context.Background(), second); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if err := adapter.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	afterRollback, _, err := readCurrentPointer(current)
	if err != nil || afterRollback != firstCurrent {
		t.Fatalf("current after rollback = %q, %v; want %q", afterRollback, err, firstCurrent)
	}
}

func TestFirstInstallFailureLeavesNoCurrentOrPartials(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("uses the running test executable as a valid linux/amd64 ELF fixture")
	}
	root := t.TempDir()
	adapter := newAdapterAt(t, domain.Target{OS: "linux", Arch: "amd64"}, root)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapter.beforeActivate = func() error { return errors.New("injected activation failure") }
	err = adapter.Install(context.Background(), fixtureBinary(t, executable, ManagedDefaultVersion))
	assertPMuxCode(t, err, pmuxerr.InstallRollbackAttempted)
	if _, err := os.Lstat(filepath.Join(root, "cli-proxy-api", "current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current exists after failure: %v", err)
	}
	versions := filepath.Join(root, "cli-proxy-api", "versions")
	assertNoFiles(t, versions)
}

type archiveEntry struct {
	name string
	body []byte
	link string
}

func makeTarGZ(path string, entries []archiveEntry) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o700, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}
		if entry.link != "" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.link
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if len(entry.body) > 0 {
			if _, err := archive.Write(entry.body); err != nil {
				return err
			}
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	return file.Close()
}

func makeZIP(path string, entries []archiveEntry) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(0o700)
		if entry.link != "" {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		body := entry.body
		if entry.link != "" {
			body = []byte(entry.link)
		}
		if _, err := writer.Write(body); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return file.Close()
}

func minimalELF(machine elf.Machine) []byte {
	body := make([]byte, 64)
	copy(body[:4], []byte("\x7fELF"))
	body[4] = byte(elf.ELFCLASS64)
	body[5] = byte(elf.ELFDATA2LSB)
	body[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(body[16:18], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(body[18:20], uint16(machine))
	binary.LittleEndian.PutUint32(body[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(body[52:54], 64)
	binary.LittleEndian.PutUint16(body[54:56], 56)
	binary.LittleEndian.PutUint16(body[58:60], 64)
	return body
}

func minimalMachO(cpu uint32) []byte {
	body := make([]byte, 32)
	binary.LittleEndian.PutUint32(body[0:4], uint32(macho.Magic64))
	binary.LittleEndian.PutUint32(body[4:8], cpu)
	binary.LittleEndian.PutUint32(body[8:12], 3)
	binary.LittleEndian.PutUint32(body[12:16], 2)
	return body
}

func minimalPE(machine uint16) []byte {
	const peOffset = 64
	body := make([]byte, 96)
	copy(body[:2], []byte("MZ"))
	binary.LittleEndian.PutUint32(body[0x3c:0x40], peOffset)
	copy(body[peOffset:peOffset+4], []byte{'P', 'E', 0, 0})
	binary.LittleEndian.PutUint16(body[peOffset+4:peOffset+6], machine)
	return body
}

func fixtureBinary(t *testing.T, source, version string) domain.ExtractedBinary {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "cli-proxy-api")
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return domain.ExtractedBinary{Path: destination, Version: version}
}

func newTestAdapter(t *testing.T, target domain.Target) *Adapter {
	t.Helper()
	return newAdapterAt(t, target, t.TempDir())
}

func newAdapterAt(t *testing.T, target domain.Target, root string) *Adapter {
	t.Helper()
	adapter, err := New(Options{Target: target, DataRoot: root, ReleaseBaseURL: "https://example.test/releases"})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func newTestAdapterWithClient(t *testing.T, target domain.Target, client *http.Client) *Adapter {
	t.Helper()
	adapter, err := New(Options{Target: target, DataRoot: t.TempDir(), HTTPClient: client, ReleaseBaseURL: "https://example.test/releases"})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func verifyArchiveForExtract(t *testing.T, adapter *Adapter, archivePath, name string) {
	t.Helper()
	body, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	asset := domain.DownloadedAsset{Path: archivePath, Name: name, SHA256: digest}
	checksums := []byte(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	if err := adapter.VerifyArchive(context.Background(), asset, checksums); err != nil {
		t.Fatalf("VerifyArchive fixture: %v", err)
	}
}

func assertPMuxCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	var structured *pmuxerr.Error
	if !errors.As(err, &structured) || structured.Code != code {
		t.Fatalf("error = %#v, want code %s", err, code)
	}
}

func assertNoPartialFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("partial staging remains: %s", entry.Name())
		}
	}
}

func assertNoFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("unexpected staging files: %v", names)
	}
}
