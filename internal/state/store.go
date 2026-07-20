package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

const SchemaVersion = 1

// Paths names the three durable PMux JSON documents. Paths must be absolute.
type Paths struct {
	Config  string
	State   string
	Secrets string
}

// Store persists PMux's versioned JSON documents. A Store has no locking side
// effects: callers acquire the operation lock only around mutations.
type Store struct {
	paths   Paths
	now     func() time.Time
	mu      sync.Mutex
	notices []RecoveryNotice
}

func New(paths Paths) (*Store, error) {
	for name, path := range map[string]string{"config": paths.Config, "state": paths.State, "secrets": paths.Secrets} {
		if path == "" || !filepath.IsAbs(path) {
			return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("%s store path must be absolute", name))
		}
	}
	return &Store{paths: paths, now: time.Now}, nil
}

func (s *Store) LoadConfig() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := Config{Version: SchemaVersion}
	exists, err := readJSON(s.paths.Config, &value)
	if err != nil {
		if isDocumentCorrupt(err) {
			return s.recoverConfig(err)
		}
		return Config{}, err
	}
	if !exists {
		return value, nil
	}
	if err := checkVersion("config", value.Version); err != nil {
		return Config{}, err
	}
	return value, nil
}

func (s *Store) SaveConfig(value Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value.Version = SchemaVersion
	if err := s.backupCurrent(s.paths.Config, validateConfigBytes); err != nil {
		return err
	}
	if err := writeJSON(s.paths.Config, value); err != nil {
		return err
	}
	return s.pruneDocumentBackups(s.paths.Config)
}

func (s *Store) LoadState() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := State{Version: SchemaVersion}
	exists, err := readJSON(s.paths.State, &value)
	if err != nil {
		if isDocumentCorrupt(err) {
			return s.recoverState(err)
		}
		return State{}, err
	}
	if !exists {
		return value, nil
	}
	if err := checkVersion("state", value.Version); err != nil {
		return State{}, err
	}
	if err := validateState(value); err != nil {
		return s.recoverState(err)
	}
	return value, nil
}

func (s *Store) SaveState(value State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value.Version = SchemaVersion
	if err := validateState(value); err != nil {
		return err
	}
	if err := s.backupCurrent(s.paths.State, validateStateBytes); err != nil {
		return err
	}
	if err := writeJSON(s.paths.State, value); err != nil {
		return err
	}
	return s.pruneDocumentBackups(s.paths.State)
}

func (s *Store) LoadSecretReferences() (SecretReferences, error) {
	value := SecretReferences{Version: SchemaVersion, Management: make(map[string]SecretReference)}
	exists, err := readJSON(s.paths.Secrets, &value)
	if err != nil {
		return SecretReferences{}, err
	}
	if !exists {
		return value, nil
	}
	if err := checkVersion("secret-reference", value.Version); err != nil {
		return SecretReferences{}, err
	}
	if err := validateSecretReferences(value); err != nil {
		return SecretReferences{}, err
	}
	if value.Management == nil {
		value.Management = make(map[string]SecretReference)
	}
	return value, nil
}

func (s *Store) SaveSecretReferences(value SecretReferences) error {
	value.Version = SchemaVersion
	if err := validateSecretReferences(value); err != nil {
		return err
	}
	return writeJSON(s.paths.Secrets, value)
}

func checkVersion(name string, version int) error {
	if version == SchemaVersion {
		return nil
	}
	if version > SchemaVersion {
		return &pmuxerr.Error{
			Code: pmuxerr.ConfigUnreadable, Class: pmuxerr.Environment,
			Message: fmt.Sprintf("pmux %s was written by a newer PMux (version %d; this binary understands %d)", name, version, SchemaVersion),
		}
	}
	return &pmuxerr.Error{
		Code: pmuxerr.ConfigUnreadable, Class: pmuxerr.Environment,
		Message: fmt.Sprintf("pmux %s has unsupported schema version %d", name, version),
	}
}

func readJSON(path string, dst any) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not open PMux state")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect PMux state")
	}
	if !info.Mode().IsRegular() {
		return false, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux state is not a regular file")
	}

	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	if err := decoder.Decode(dst); err != nil {
		return false, &documentCorruptError{err: pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux state contains invalid JSON")}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return false, &documentCorruptError{err: pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux state contains trailing data")}
	}
	return true, nil
}

func writeJSON(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not encode PMux state")
	}
	payload = append(payload, '\n')
	return writeBytes(path, payload)
}

func writeBytes(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create PMux state directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect PMux state directory")
	}
	tmp, err := os.CreateTemp(dir, ".pmux-json-*")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create temporary PMux state")
	}
	name := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect temporary PMux state")
	}
	if _, err := tmp.Write(payload); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not write temporary PMux state")
	}
	if err := tmp.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush temporary PMux state")
	}
	if err := tmp.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close temporary PMux state")
	}
	if err := replaceStateFile(name, path); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not atomically replace PMux state")
	}
	if err := syncDirectory(dir); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush PMux state directory")
	}
	committed = true
	return nil
}

func syncDirectory(path string) error {
	return syncStateDirectory(path)
}

func (s *Store) backupCurrent(path string, validate func([]byte) error) error {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not read PMux state before backup")
	}
	if err := validate(payload); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "existing PMux state is invalid; refusing to replace it before recovery")
	}
	stamp := s.now().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%s.%s.%s.bak", filepath.Base(path), stamp, sha8(payload))
	return writeExclusivePrivate(filepath.Join(documentBackupDir(path), name), payload)
}

func (s *Store) latestValidBackup(path string, validate func([]byte) error) ([]byte, string, bool, error) {
	entries, err := os.ReadDir(documentBackupDir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not list PMux state backups")
	}
	prefix := filepath.Base(path) + "."
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".bak") {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		backup := filepath.Join(documentBackupDir(path), name)
		payload, err := os.ReadFile(backup)
		if err != nil {
			continue
		}
		if err := validate(payload); err == nil {
			return payload, backup, true, nil
		}
	}
	return nil, "", false, nil
}

func (s *Store) pruneDocumentBackups(path string) error {
	const keep = 10
	entries, err := os.ReadDir(documentBackupDir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not list PMux state backups for retention")
	}
	prefix := filepath.Base(path) + "."
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".bak") {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names[minimum(len(names), keep):] {
		if err := os.Remove(filepath.Join(documentBackupDir(path), name)); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not prune PMux state backup")
		}
	}
	if len(names) > keep {
		if err := syncDirectory(documentBackupDir(path)); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush PMux state backup retention")
		}
	}
	return nil
}

func writeExclusivePrivate(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create private recovery directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect private recovery directory")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not create private recovery artifact")
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect private recovery artifact")
	}
	if _, err := file.Write(payload); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not write private recovery artifact")
	}
	if err := file.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private recovery artifact")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close private recovery artifact")
	}
	if err := syncDirectory(dir); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush private recovery directory")
	}
	complete = true
	return nil
}

func sha8(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:4])
}

func minimum(left, right int) int {
	if left < right {
		return left
	}
	return right
}
