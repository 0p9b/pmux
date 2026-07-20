package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

const DefaultBackupRetention = 10

type Backups struct {
	root string
	keep int
	now  func() time.Time
}

func NewBackups(root string, keep int) (*Backups, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "backup root must be absolute")
	}
	if keep < 0 {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "backup retention cannot be negative")
	}
	if keep == 0 {
		keep = DefaultBackupRetention
	}
	return &Backups{root: root, keep: keep, now: time.Now}, nil
}

func (b *Backups) Create(instanceID, baseName string, payload []byte) (string, error) {
	if err := validComponent(instanceID); err != nil {
		return "", err
	}
	if err := validComponent(baseName); err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	path := filepath.Join(b.root, instanceID, fmt.Sprintf("%s.%s.%s.bak", baseName, b.now().UTC().Format("20060102T150405Z"), hex.EncodeToString(digest[:4])))
	if err := WritePrivateExclusive(path, payload); err != nil {
		return "", err
	}
	return path, nil
}

// List returns matching canonical backups newest-first, with the filename as a
// deterministic tie breaker for same-second backups.
func (b *Backups) List(instanceID, baseName string) ([]string, error) {
	if err := validComponent(instanceID); err != nil {
		return nil, err
	}
	if err := validComponent(baseName); err != nil {
		return nil, err
	}
	dir := filepath.Join(b.root, instanceID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not list backups")
	}
	prefix := baseName + "."
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".bak") {
			result = append(result, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Slice(result, func(i, j int) bool { return filepath.Base(result[i]) > filepath.Base(result[j]) })
	return result, nil
}

// Prune removes all but the newest retention count. It is intentionally a
// separate operation so callers run it only after the protected mutation has
// been verified successfully.
func (b *Backups) Prune(instanceID, baseName string) ([]string, error) {
	all, err := b.List(instanceID, baseName)
	if err != nil {
		return nil, err
	}
	if len(all) <= b.keep {
		return nil, nil
	}
	removed := make([]string, 0, len(all)-b.keep)
	for _, path := range all[b.keep:] {
		if err := os.Remove(path); err != nil {
			return removed, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not prune old backup")
		}
		removed = append(removed, path)
	}
	if len(removed) != 0 {
		if err := SyncDirectory(filepath.Join(b.root, instanceID)); err != nil {
			return removed, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush backup retention")
		}
	}
	return removed, nil
}

func validComponent(value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "backup name contains an invalid path component")
	}
	return nil
}
