package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

type Kind string

const (
	KindText     Kind = "text"
	KindJSON     Kind = "json"
	KindAuthFile Kind = "auth-file"
)

// Entry data is supplied by explicitly allowlisted collectors. Auth-file data
// is always discarded regardless of archive path or caller intent.
type Entry struct {
	ArchivePath string
	SourcePath  string
	Kind        Kind
	Data        []byte
}

type ManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Kind   Kind   `json:"kind"`
}

type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	CreatedAt     time.Time       `json:"created_at"`
	Entries       []ManifestEntry `json:"entries"`
	Excluded      map[string]int  `json:"excluded"`
}

type Result struct {
	Path     string   `json:"path"`
	SHA256   string   `json:"sha256"`
	Manifest Manifest `json:"manifest"`
}

type PermissionFunc func(path string, isDir bool) error

// Builder has deliberately no include-auth or redaction-bypass option.
type Builder struct {
	AuthRoots               []string
	KnownSecrets            []string
	Now                     func() time.Time
	SecurePermissions       PermissionFunc
	VerifySecurePermissions PermissionFunc
}

var (
	bearerPattern    = regexp.MustCompile(`(?i)(bearer\s+)[^\s,;"'}]+`)
	headerPattern    = regexp.MustCompile(`(?i)("?(?:authorization|x-management-key)"?\s*:\s*)[^\s,;"'}]+`)
	envPattern       = regexp.MustCompile(`(?i)(ANTHROPIC_AUTH_TOKEN|OPENAI_API_KEY|GEMINI_API_KEY)(\s*=\s*)[^\s"'}]+`)
	keyPattern       = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	fieldPattern     = regexp.MustCompile(`(?i)("?(?:access_token|refresh_token|id_token|api_key|api-key|secret-key|secret_key|private_key|password)"?\s*[:=]\s*")([^"]+)(")`)
	bareFieldPattern = regexp.MustCompile(`(?i)("?(?:access_token|refresh_token|id_token|api_key|api-key|secret-key|secret_key|private_key|password)"?\s*[:=]\s*)([^"\s,}\]]+)`)
	pemPattern       = regexp.MustCompile(`(?s)-----BEGIN [^\r\n-]*PRIVATE KEY-----.*?-----END [^\r\n-]*PRIVATE KEY-----`)
)

func (b Builder) Build(ctx context.Context, destination string, entries []Entry) (result Result, retErr error) {
	if err := ctx.Err(); err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.CodeInterrupted, pmuxerr.Environment, "diagnostic bundle creation was interrupted")
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "diagnostic bundle destination is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "diagnostic bundle directory could not be created")
	}

	now := time.Now().UTC()
	if b.Now != nil {
		now = b.Now().UTC()
	}
	manifest := Manifest{SchemaVersion: 1, CreatedAt: now, Entries: []ManifestEntry{}, Excluded: map[string]int{}}
	type stagedEntry struct {
		name string
		kind Kind
		data []byte
	}
	staged := make([]stagedEntry, 0, len(entries)+1)
	seen := make(map[string]bool)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Result{}, pmuxerr.Wrap(err, pmuxerr.CodeInterrupted, pmuxerr.Environment, "diagnostic bundle creation was interrupted")
		}
		reason := b.exclusionReason(entry)
		if reason != "" {
			manifest.Excluded[reason]++
			continue
		}
		name, nameErr := cleanArchivePath(entry.ArchivePath)
		if nameErr != nil {
			return Result{}, nameErr
		}
		if secret := b.firstCanary([]byte(name)); secret != "" {
			return Result{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "diagnostic bundle archive path contains a known secret")
		}
		if name == "MANIFEST.json" || seen[name] {
			return Result{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "diagnostic bundle contains a duplicate archive path")
		}
		seen[name] = true
		data := b.redact(entry.Data)
		if secret := b.firstCanary(data); secret != "" {
			return Result{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "diagnostic bundle redaction failed closed because a known secret remained")
		}
		hash := sha256.Sum256(data)
		kind := entry.Kind
		if kind == "" {
			kind = KindText
		}
		manifest.Entries = append(manifest.Entries, ManifestEntry{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:]), Kind: kind})
		staged = append(staged, stagedEntry{name: name, kind: kind, data: data})
	}
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	sort.Slice(staged, func(i, j int) bool { return staged[i].name < staged[j].name })
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "diagnostic bundle manifest could not be encoded")
	}
	manifestBytes = append(manifestBytes, '\n')
	if secret := b.firstCanary(manifestBytes); secret != "" {
		return Result{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "diagnostic bundle manifest contains a known secret")
	}
	staged = append(staged, stagedEntry{name: "MANIFEST.json", kind: KindJSON, data: manifestBytes})

	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "diagnostic bundle destination already exists or cannot be created")
	}
	committed := false
	defer func() {
		if !committed {
			_ = file.Close()
			_ = os.Remove(absolute)
		}
	}()
	if b.SecurePermissions != nil {
		if err := b.SecurePermissions(absolute, false); err != nil {
			return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "diagnostic bundle could not be made private")
		}
	} else if err := os.Chmod(absolute, 0o600); err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "diagnostic bundle could not be made private")
	}
	zw := zip.NewWriter(file)
	for _, entry := range staged {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(0o600)
		header.Modified = now
		writer, createErr := zw.CreateHeader(header)
		if createErr != nil {
			return Result{}, pmuxerr.Wrap(createErr, pmuxerr.UnhandledInternal, pmuxerr.Internal, "diagnostic bundle entry could not be created")
		}
		if _, writeErr := writer.Write(entry.data); writeErr != nil {
			return Result{}, pmuxerr.Wrap(writeErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "diagnostic bundle entry could not be written")
		}
	}
	if err := zw.Close(); err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "diagnostic bundle archive could not be finalized")
	}
	if err := file.Sync(); err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "diagnostic bundle archive could not be flushed")
	}
	if err := file.Close(); err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "diagnostic bundle archive could not be closed")
	}
	if b.VerifySecurePermissions != nil {
		if err := b.VerifySecurePermissions(absolute, false); err != nil {
			return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "diagnostic bundle privacy verification failed")
		}
	}
	archiveBytes, err := os.ReadFile(absolute)
	if err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "diagnostic bundle archive could not be verified")
	}
	if err := b.verifyArchive(archiveBytes); err != nil {
		return Result{}, err
	}
	archiveHash := sha256.Sum256(archiveBytes)
	committed = true
	return Result{Path: absolute, SHA256: hex.EncodeToString(archiveHash[:]), Manifest: manifest}, nil
}

func (b Builder) exclusionReason(entry Entry) string {
	if entry.Kind == KindAuthFile {
		return "auth-file-content"
	}
	archivePath := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(entry.ArchivePath)), "./")
	firstSegment := strings.ToLower(strings.SplitN(archivePath, "/", 2)[0])
	if firstSegment == "auth" || firstSegment == "auth-files" {
		return "auth-file-content"
	}
	if entry.SourcePath != "" {
		for _, root := range b.AuthRoots {
			if within(root, entry.SourcePath) {
				return "auth-file-content"
			}
		}
		base := strings.ToLower(filepath.Base(entry.SourcePath))
		if base == "api-key.txt" || base == "secrets.json" {
			return "secret-store"
		}
		if strings.Contains(filepath.ToSlash(entry.SourcePath), "/backups/") {
			return "secret-bearing-backup"
		}
	}
	return ""
}

func within(root, path string) bool {
	rootAbs, rootErr := filepath.Abs(root)
	pathAbs, pathErr := filepath.Abs(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != "" && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func cleanArchivePath(name string) (string, error) {
	if strings.Contains(name, `\`) {
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "diagnostic bundle archive path is unsafe")
	}
	name = filepath.ToSlash(strings.TrimSpace(name))
	clean := filepath.ToSlash(filepath.Clean(name))
	if name == "" || clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "diagnostic bundle archive path is unsafe")
	}
	return clean, nil
}

func (b Builder) redact(data []byte) []byte {
	text := sanitizeControls(string(data))
	text = redact.Known(text, b.KnownSecrets...)
	text = bearerPattern.ReplaceAllString(text, `$1<redacted>`)
	text = headerPattern.ReplaceAllString(text, `$1<redacted>`)
	text = envPattern.ReplaceAllString(text, `$1$2<redacted>`)
	text = keyPattern.ReplaceAllStringFunc(text, redact.Mask)
	text = fieldPattern.ReplaceAllString(text, `$1<redacted>$3`)
	text = bareFieldPattern.ReplaceAllString(text, `$1<redacted>`)
	text = pemPattern.ReplaceAllString(text, "<redacted-private-key>")
	return []byte(text)
}

func sanitizeControls(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' || (unicode.IsPrint(r) && r != '\x1b') {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (b Builder) firstCanary(data []byte) string {
	for _, secret := range b.KnownSecrets {
		if secret != "" && bytes.Contains(data, []byte(secret)) {
			return secret
		}
	}
	return ""
}

func (b Builder) verifyArchive(data []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Internal, "diagnostic bundle archive verification failed")
	}
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		rc, openErr := file.Open()
		if openErr != nil {
			return pmuxerr.Wrap(openErr, pmuxerr.InstallIntegrityFailed, pmuxerr.Internal, "diagnostic bundle entry verification failed")
		}
		entryData, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return pmuxerr.Wrap(readErr, pmuxerr.InstallIntegrityFailed, pmuxerr.Internal, "diagnostic bundle entry verification failed")
		}
		if closeErr != nil {
			return pmuxerr.Wrap(closeErr, pmuxerr.InstallIntegrityFailed, pmuxerr.Internal, "diagnostic bundle entry verification failed")
		}
		if secret := b.firstCanary(entryData); secret != "" {
			return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, fmt.Sprintf("diagnostic bundle entry %q contains a known secret", file.Name))
		}
	}
	return nil
}
