package installer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	domain "github.com/0p9b/pmux/internal/domain/install"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	recoverySchemaVersion = 1
	recoveryFileName      = ".install-pending.json"
)

type recoveryStage string

const (
	stagePlanned           recoveryStage = "planned"
	stageDownloading       recoveryStage = "downloading"
	stageDownloaded        recoveryStage = "downloaded"
	stageChecksumVerified  recoveryStage = "checksum_verified"
	stageExtracting        recoveryStage = "extracting"
	stageExtracted         recoveryStage = "extracted"
	stageInstalling        recoveryStage = "installing"
	stageActivated         recoveryStage = "activated"
	stageConfigCheckpoint  recoveryStage = "config_checkpointed"
	stageServiceCheckpoint recoveryStage = "service_checkpointed"
)

// ConfigCheckpoint records enough non-secret state to restore a setup config
// mutation. BackupPath must refer to exact pre-mutation bytes.
type ConfigCheckpoint struct {
	Path       string `json:"path"`
	BackupPath string `json:"backup_path"`
}

// ServiceCheckpoint records the native identity and its pre-mutation artifact.
// RestoreService is supplied by the composition root because installer does not
// import platform service adapters.
type ServiceCheckpoint struct {
	Identity     string `json:"identity"`
	BackupPath   string `json:"backup_path,omitempty"`
	WasInstalled bool   `json:"was_installed"`
	WasRunning   bool   `json:"was_running"`
}

type recoveryRecord struct {
	Schema         int                `json:"schema"`
	UpdatedAt      time.Time          `json:"updated_at"`
	Stage          recoveryStage      `json:"stage"`
	Target         domain.Target      `json:"target"`
	Version        string             `json:"version,omitempty"`
	AssetName      string             `json:"asset_name,omitempty"`
	AssetPath      string             `json:"asset_path,omitempty"`
	PartialPath    string             `json:"partial_path,omitempty"`
	AssetSHA256    string             `json:"asset_sha256,omitempty"`
	ExpectedSHA256 string             `json:"expected_sha256,omitempty"`
	ExtractStage   string             `json:"extract_stage,omitempty"`
	ExtractedPath  string             `json:"extracted_path,omitempty"`
	VersionPath    string             `json:"version_path,omitempty"`
	CreatedVersion bool               `json:"created_version,omitempty"`
	CurrentPath    string             `json:"current_path,omitempty"`
	PriorCurrent   string             `json:"prior_current,omitempty"`
	PriorExists    bool               `json:"prior_exists,omitempty"`
	Activated      bool               `json:"activated,omitempty"`
	Config         *ConfigCheckpoint  `json:"config,omitempty"`
	Service        *ServiceCheckpoint `json:"service,omitempty"`
}

// BeginSetup holds the durable recovery record across installer, config, and
// service checkpoints. The caller must invoke Complete only after end-to-end
// setup verification succeeds.
func (a *Adapter) BeginSetup(ctx context.Context, release domain.Release) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if _, err := os.Lstat(a.recoveryPath()); err == nil {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "a managed install recovery record is already pending; recover it before starting another install")
	} else if !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not inspect managed install recovery state")
	}
	a.recoveryHeld = true
	return a.writeRecoveryLocked(recoveryRecord{
		Schema:    recoverySchemaVersion,
		Stage:     stagePlanned,
		Target:    a.target,
		Version:   release.Version,
		AssetName: release.AssetName,
	})
}

func (a *Adapter) CheckpointConfig(ctx context.Context, checkpoint ConfigCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	path, err := absoluteRequired(checkpoint.Path, "config path")
	if err != nil {
		return err
	}
	backup, err := absoluteRequired(checkpoint.BackupPath, "config backup path")
	if err != nil {
		return err
	}
	checkpoint.Path, checkpoint.BackupPath = path, backup
	return a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageConfigCheckpoint
		record.Config = &checkpoint
	})
}

func (a *Adapter) CheckpointService(ctx context.Context, checkpoint ServiceCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	if checkpoint.Identity == "" {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "service recovery identity is required")
	}
	if checkpoint.BackupPath != "" {
		backup, err := absoluteRequired(checkpoint.BackupPath, "service backup path")
		if err != nil {
			return err
		}
		checkpoint.BackupPath = backup
	}
	return a.updateRecovery(func(record *recoveryRecord) {
		record.Stage = stageServiceCheckpoint
		record.Service = &checkpoint
	})
}

// Complete removes durable recovery state only after the caller has verified
// all intended setup effects.
func (a *Adapter) Complete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if err := removeRecoveryFile(a.recoveryPath()); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not clear completed managed install recovery state")
	}
	a.recoveryHeld = false
	return nil
}

// Recover rolls back a pending managed install using only exact paths and
// fingerprints durably recorded before side effects. It never reads or writes
// secret values into the recovery record.
func (a *Adapter) Recover(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	record, exists, err := a.readRecoveryLocked()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if record.Target != a.target {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "pending managed install target differs from this installer target")
	}
	if record.Service != nil {
		if a.restoreService == nil {
			return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "pending managed install requires service restoration, but no service recovery callback is configured")
		}
		if err := a.restoreService(ctx, *record.Service); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not restore service state from pending managed install")
		}
	}
	if record.Config != nil {
		if err := restoreExactFile(ctx, record.Config.BackupPath, record.Config.Path); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not restore config from pending managed install")
		}
	}
	if record.Activated && record.CurrentPath != "" {
		if record.PriorExists {
			if err := writeCurrentPointer(filepath.Dir(record.CurrentPath), record.CurrentPath, record.PriorCurrent); err != nil {
				return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not restore prior current pointer from pending managed install")
			}
		} else if err := removeCurrentPointer(record.CurrentPath); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not remove interrupted first-install current pointer")
		}
	}
	if record.CreatedVersion && record.VersionPath != "" {
		if err := removeManagedPath(a.dataRoot, record.VersionPath, true); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not remove interrupted managed version")
		}
	}
	if record.ExtractStage != "" {
		if err := removeManagedPath(a.dataRoot, record.ExtractStage, true); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not remove interrupted extraction staging")
		}
	}
	if record.PartialPath != "" {
		if err := removeManagedPath(filepath.Dir(record.PartialPath), record.PartialPath, false); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not remove interrupted download staging")
		}
	}
	if record.AssetPath != "" && record.Stage != stageChecksumVerified && record.Stage != stageExtracting && record.Stage != stageExtracted && record.Stage != stageInstalling && record.Stage != stageActivated && record.Stage != stageConfigCheckpoint && record.Stage != stageServiceCheckpoint {
		if err := removeManagedPath(filepath.Dir(record.AssetPath), record.AssetPath, false); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "could not remove unverified downloaded archive")
		}
	}
	if err := removeRecoveryFile(a.recoveryPath()); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "recovery succeeded but its durable record could not be cleared")
	}
	a.recoveryHeld = false
	a.activated = false
	return nil
}

func (a *Adapter) recoveryPath() string {
	return filepath.Join(a.dataRoot, "cli-proxy-api", recoveryFileName)
}

func (a *Adapter) updateRecovery(change func(*recoveryRecord)) error {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	record, exists, err := a.readRecoveryLocked()
	if err != nil {
		return err
	}
	if !exists {
		record = recoveryRecord{Schema: recoverySchemaVersion, Target: a.target}
	}
	change(&record)
	return a.writeRecoveryLocked(record)
}

func (a *Adapter) readRecoveryLocked() (recoveryRecord, bool, error) {
	var record recoveryRecord
	body, err := os.ReadFile(a.recoveryPath())
	if errors.Is(err, os.ErrNotExist) {
		return record, false, nil
	}
	if err != nil {
		return record, false, pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not read managed install recovery state")
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return record, false, pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "managed install recovery state is invalid")
	}
	if record.Schema != recoverySchemaVersion {
		return record, false, pmuxerr.New(pmuxerr.JournalCorrupt, pmuxerr.Environment, fmt.Sprintf("managed install recovery schema %d is unsupported", record.Schema))
	}
	return record, true, nil
}

func (a *Adapter) writeRecoveryLocked(record recoveryRecord) error {
	record.Schema = recoverySchemaVersion
	record.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(record)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Internal, "could not encode managed install recovery state")
	}
	path := a.recoveryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not create managed install recovery directory")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".install-recovery-")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not stage managed install recovery state")
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceRecoveryFile(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return syncDir(filepath.Dir(path))
}

func removeRecoveryFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return syncDir(filepath.Dir(path))
}

func absoluteRequired(value, label string) (string, error) {
	if value == "" {
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, label+" is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "could not resolve "+label)
	}
	return absolute, nil
}

func restoreExactFile(ctx context.Context, backup, destination string) error {
	input, err := os.Open(backup)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".recover-config-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := io.Copy(temporary, &contextReader{ctx: ctx, r: input}); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceRecoveryFile(temporaryPath, destination); err != nil {
		return err
	}
	committed = true
	return syncDir(filepath.Dir(destination))
}

func removeManagedPath(root, candidate string, directory bool) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return errors.New("recovery path escapes its managed root")
	}
	if directory {
		return os.RemoveAll(absoluteCandidate)
	}
	if err := os.Remove(absoluteCandidate); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func digestHex(digest [32]byte) string {
	return hex.EncodeToString(digest[:])
}
