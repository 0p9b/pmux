package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type RecoveryNotice struct {
	Kind         string `json:"kind"`
	CorruptPath  string `json:"corrupt_path"`
	BackupPath   string `json:"backup_path"`
	Message      string `json:"message"`
}

// RecoveryNeeded tells runtime composition that no valid state backup exists.
// Runtime may rebuild only from the canonical managed layout; it must not call
// broad home/process/service discovery. Cause is retained for verbose output.
type RecoveryNeeded struct {
	Kind        string
	CorruptPath string
	Cause       error
}

func (e *RecoveryNeeded) Error() string {
	return fmt.Sprintf("pmux %s was corrupt and no valid backup is available; rebuild from the canonical managed layout only. The corrupt file was saved as %s.", e.Kind, e.CorruptPath)
}

func (e *RecoveryNeeded) Unwrap() error {
	return e.Cause
}

func (e *RecoveryNeeded) RebuiltMessage() string {
	return fmt.Sprintf("pmux state was corrupt and has been rebuilt from the CLIProxyAPI installation; favorites and recent launches were reset. The corrupt file was saved as %s.", e.CorruptPath)
}

// RecoveryNotices returns and clears successful automatic-restoration notices.
func (s *Store) RecoveryNotices() []RecoveryNotice {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]RecoveryNotice(nil), s.notices...)
	s.notices = nil
	return result
}

// RebuildState completes a typed RecoveryNeeded after runtime has inspected
// only the canonical managed layout. User preferences that cannot be
// reconstructed are reset unconditionally.
func (s *Store) RebuildState(value State, recovery *RecoveryNeeded) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if recovery == nil || recovery.Kind != "state" || recovery.CorruptPath == "" {
		return fmt.Errorf("state rebuild requires its typed recovery record")
	}
	value.Version = SchemaVersion
	value.Favorites = nil
	value.RecentModels = nil
	if err := validateState(value); err != nil {
		return err
	}
	if err := writeJSON(s.paths.State, value); err != nil {
		return err
	}
	s.notices = append(s.notices, RecoveryNotice{
		Kind: "state", CorruptPath: recovery.CorruptPath, Message: recovery.RebuiltMessage(),
	})
	return nil
}

// ResetConfig completes config recovery when no valid backup exists. PMux
// preferences safely return to defaults; installation discovery is untouched.
func (s *Store) ResetConfig(recovery *RecoveryNeeded) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if recovery == nil || recovery.Kind != "config" || recovery.CorruptPath == "" {
		return fmt.Errorf("config reset requires its typed recovery record")
	}
	if err := writeJSON(s.paths.Config, Config{Version: SchemaVersion}); err != nil {
		return err
	}
	s.notices = append(s.notices, RecoveryNotice{
		Kind: "config", CorruptPath: recovery.CorruptPath,
		Message: fmt.Sprintf("pmux config was corrupt and reset to defaults. The corrupt file was saved as %s.", recovery.CorruptPath),
	})
	return nil
}

type documentCorruptError struct {
	err error
}

func (e *documentCorruptError) Error() string { return e.err.Error() }
func (e *documentCorruptError) Unwrap() error { return e.err }

func isDocumentCorrupt(err error) bool {
	var target *documentCorruptError
	return errors.As(err, &target)
}

func validateConfigBytes(payload []byte) error {
	var value Config
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	return checkVersion("config", value.Version)
}

func validateStateBytes(payload []byte) error {
	var value State
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	if err := checkVersion("state", value.Version); err != nil {
		return err
	}
	return validateState(value)
}

func (s *Store) recoverConfig(cause error) (Config, error) {
	artifact, err := s.preserveCorrupt(s.paths.Config)
	if err != nil {
		return Config{}, err
	}
	payload, backup, found, err := s.latestValidBackup(s.paths.Config, validateConfigBytes)
	if err != nil {
		return Config{}, err
	}
	if !found {
		return Config{}, &RecoveryNeeded{Kind: "config", CorruptPath: artifact, Cause: cause}
	}
	if err := writeRawJSON(s.paths.Config, payload); err != nil {
		return Config{}, err
	}
	var value Config
	if _, err := readJSON(s.paths.Config, &value); err != nil {
		return Config{}, err
	}
	if err := checkVersion("config", value.Version); err != nil {
		return Config{}, err
	}
	s.notices = append(s.notices, RecoveryNotice{
		Kind: "config", CorruptPath: artifact, BackupPath: backup,
		Message: fmt.Sprintf("pmux config was corrupt and restored from backup %s. The corrupt file was saved as %s.", backup, artifact),
	})
	return value, nil
}

func (s *Store) recoverState(cause error) (State, error) {
	artifact, err := s.preserveCorrupt(s.paths.State)
	if err != nil {
		return State{}, err
	}
	payload, backup, found, err := s.latestValidBackup(s.paths.State, validateStateBytes)
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{}, &RecoveryNeeded{Kind: "state", CorruptPath: artifact, Cause: cause}
	}
	if err := writeRawJSON(s.paths.State, payload); err != nil {
		return State{}, err
	}
	var value State
	if _, err := readJSON(s.paths.State, &value); err != nil {
		return State{}, err
	}
	if err := validateState(value); err != nil {
		return State{}, err
	}
	s.notices = append(s.notices, RecoveryNotice{
		Kind: "state", CorruptPath: artifact, BackupPath: backup,
		Message: fmt.Sprintf("pmux state was corrupt and restored from backup %s. The corrupt file was saved as %s.", backup, artifact),
	})
	return value, nil
}

func writeRawJSON(path string, payload []byte) error {
	var document any
	if err := json.Unmarshal(payload, &document); err != nil {
		return err
	}
	return writeBytes(path, payload)
}

func (s *Store) preserveCorrupt(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha8(payload)
	stamp := s.now().UTC().Format("20060102T150405.000000000Z")
	artifact := fmt.Sprintf("%s.corrupt.%s.%s", path, stamp, digest)
	if err := writeExclusivePrivate(artifact, payload); err != nil {
		return "", err
	}
	return artifact, nil
}

func documentBackupDir(path string) string {
	return filepath.Join(filepath.Dir(path), "backups", "pmux")
}
