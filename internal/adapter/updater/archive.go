package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func verifyArchiveChecksum(archivePath, checksumsPath, archiveName string) error {
	data, err := os.ReadFile(checksumsPath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "Could not read checksums.txt.")
	}
	var expected string
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" { continue }
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != archiveName { continue }
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return &pmuxerr.Error{Code: pmuxerr.InstallIntegrityFailed, Class: pmuxerr.Upstream, Message: "The matching checksum entry is malformed.", Evidence: []string{fmt.Sprintf("checksums.txt line %d", lineNo+1)}}
		}
		if expected != "" {
			return &pmuxerr.Error{Code: pmuxerr.InstallIntegrityFailed, Class: pmuxerr.Upstream, Message: "checksums.txt contains duplicate entries for the platform archive."}
		}
		expected = strings.ToLower(fields[0])
	}
	if expected == "" {
		return &pmuxerr.Error{Code: pmuxerr.InstallIntegrityFailed, Class: pmuxerr.Upstream, Message: "checksums.txt has no entry for the platform archive; extraction was refused.", Evidence: []string{archiveName}}
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "Could not read the downloaded archive.")
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil { return pmuxerr.Wrap(copyErr, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "Could not hash the downloaded archive.") }
	if closeErr != nil { return pmuxerr.Wrap(closeErr, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "Could not close the downloaded archive.") }
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return &pmuxerr.Error{Code: pmuxerr.InstallIntegrityFailed, Class: pmuxerr.Upstream, Message: "Archive checksum mismatch; extraction was refused.", Evidence: []string{"expected sha256 " + expected, "actual sha256 " + actual}}
	}
	return nil
}

func extractExecutable(archivePath, destination, executableName string) (string, error) {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "Could not create private extraction staging.")
	}
	var err error
	switch {
	case strings.HasSuffix(archivePath, ".zip"):
		err = extractZipExecutable(archivePath, destination, executableName)
	case strings.HasSuffix(archivePath, ".tar.gz"), strings.HasSuffix(archivePath, ".tgz"):
		err = extractTarExecutable(archivePath, destination, executableName)
	default:
		err = errors.New("unsupported release archive format")
	}
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Upstream, "Verified archive could not be extracted safely.")
	}
	return filepath.Join(destination, executableName), nil
}

func cleanArchiveName(name string) (string, error) {
	if strings.ContainsRune(name, '\\') || path.IsAbs(name) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, nil
}

func extractTarExecutable(archivePath, destination, executableName string) error {
	f, err := os.Open(archivePath)
	if err != nil { return err }
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil { return err }
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) { break }
		if err != nil { return err }
		clean, err := cleanArchiveName(h.Name)
		if err != nil { return err }
		if path.Base(clean) != executableName { continue }
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA { return fmt.Errorf("executable is not a regular file") }
		if found { return fmt.Errorf("archive contains more than one %s", executableName) }
		if err := writeExtracted(filepath.Join(destination, executableName), tr, h.Size); err != nil { return err }
		found = true
	}
	if !found { return fmt.Errorf("archive does not contain %s", executableName) }
	return nil
}

func extractZipExecutable(archivePath, destination, executableName string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil { return err }
	defer zr.Close()
	found := false
	for _, entry := range zr.File {
		clean, err := cleanArchiveName(entry.Name)
		if err != nil { return err }
		if path.Base(clean) != executableName { continue }
		if !entry.Mode().IsRegular() { return fmt.Errorf("executable is not a regular file") }
		if found { return fmt.Errorf("archive contains more than one %s", executableName) }
		r, err := entry.Open()
		if err != nil { return err }
		err = writeExtracted(filepath.Join(destination, executableName), r, int64(entry.UncompressedSize64))
		closeErr := r.Close()
		if err != nil { return err }
		if closeErr != nil { return closeErr }
		found = true
	}
	if !found { return fmt.Errorf("archive does not contain %s", executableName) }
	return nil
}

func writeExtracted(destination string, source io.Reader, size int64) error {
	if size < 0 || size > defaultMaxDownload { return fmt.Errorf("executable has invalid size %d", size) }
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil { return err }
	written, copyErr := io.Copy(f, io.LimitReader(source, size+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil { return copyErr }
	if written != size { return fmt.Errorf("executable size mismatch: expected %d, wrote %d", size, written) }
	if syncErr != nil { return syncErr }
	return closeErr
}

func verifyExecutable(filename string, target Target) error {
	if target.GOOS == "" || target.Arch == "" { target = NativeTarget() }
	var arch string
	switch target.GOOS {
	case "linux":
		f, err := elf.Open(filename)
		if err != nil { return unsupportedExecutable(target, err) }
		defer f.Close()
		switch f.Machine {
		case elf.EM_X86_64: arch = "amd64"
		case elf.EM_AARCH64: arch = "arm64"
		default: arch = f.Machine.String()
		}
	case "darwin":
		f, err := macho.Open(filename)
		if err != nil { return unsupportedExecutable(target, err) }
		defer f.Close()
		switch f.Cpu {
		case macho.CpuAmd64: arch = "amd64"
		case macho.CpuArm64: arch = "arm64"
		default: arch = f.Cpu.String()
		}
	case "windows":
		f, err := pe.Open(filename)
		if err != nil { return unsupportedExecutable(target, err) }
		defer f.Close()
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64: arch = "amd64"
		case pe.IMAGE_FILE_MACHINE_ARM64: arch = "arm64"
		default: arch = fmt.Sprintf("0x%x", f.Machine)
		}
	default:
		return unsupportedExecutable(target, fmt.Errorf("unsupported OS %q", target.GOOS))
	}
	if arch != target.Arch {
		return unsupportedExecutable(target, fmt.Errorf("executable architecture is %s", arch))
	}
	return nil
}

func unsupportedExecutable(target Target, cause error) error {
	return &pmuxerr.Error{Code: pmuxerr.InstallUnsupportedTarget, Class: pmuxerr.Upstream, Message: "Release executable does not match the requested platform.", Evidence: []string{target.GOOS + "/" + target.Arch}, Cause: cause}
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" { return nil }
	f, err := os.Open(dir)
	if err != nil { return err }
	defer f.Close()
	return f.Sync()
}
