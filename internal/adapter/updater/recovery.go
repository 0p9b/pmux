package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	adapterlock "github.com/0p9b/pmux/internal/adapter/lock"
	"github.com/0p9b/pmux/internal/domain/update"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const recoveryVersion = 1

type recoveryRecord struct {
	Version        int              `json:"version"`
	Component      update.Component `json:"component"`
	Phase          string           `json:"phase"`
	Workspace      string           `json:"workspace,omitempty"`
	ArchivePath    string           `json:"archive_path,omitempty"`
	ArchiveSHA256  string           `json:"archive_sha256,omitempty"`
	CandidatePath  string           `json:"candidate_path,omitempty"`
	CandidateSHA256 string          `json:"candidate_sha256,omitempty"`
	CurrentPath    string           `json:"current_path,omitempty"`
	PreviousPath   string           `json:"previous_path,omitempty"`
	CurrentSHA256  string           `json:"current_sha256,omitempty"`
	PointerPath    string           `json:"pointer_path,omitempty"`
	OldTarget      string           `json:"old_target,omitempty"`
	NewTarget      string           `json:"new_target,omitempty"`
	FinalDir       string           `json:"final_dir,omitempty"`
	WasRunning     bool             `json:"was_running,omitempty"`
	OldVersion     string           `json:"old_version,omitempty"`
	NewVersion     string           `json:"new_version,omitempty"`
	StopTimeoutNS  int64            `json:"stop_timeout_ns,omitempty"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type recoveryManager struct {
	path   string
	locker MutationLocker
}

func (e *Engine) recoveryFor(component update.Component, adjacent string) (*recoveryManager, error) {
	path := e.recoveryPath
	if path == "" {
		name := ".pmux-proxy-update-pending.json"
		if component == update.Self {
			name = ".pmux-self-update-pending.json"
		}
		path = filepath.Join(adjacent, name)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "Update recovery path is invalid.")
	}
	locker := e.recoveryLocker
	if locker == nil {
		manager, err := adapterlock.New(filepath.Join(filepath.Dir(absolute), "pmux.lock"))
		if err != nil {
			return nil, err
		}
		locker = manager
	}
	return &recoveryManager{path: absolute, locker: locker}, nil
}

func (m *recoveryManager) withMutation(ctx context.Context, operation string, mutate func() error) error {
	return m.locker.WithMutation(ctx, operation, mutate)
}

func (m *recoveryManager) begin(component update.Component, oldVersion string) error {
	return m.write(recoveryRecord{Version: recoveryVersion, Component: component, Phase: "started", OldVersion: oldVersion})
}

func (m *recoveryManager) checkpoint(change func(*recoveryRecord)) error {
	record, err := m.read()
	if err != nil {
		return err
	}
	if record == nil {
		return pmuxerr.New(pmuxerr.JournalCorrupt, pmuxerr.Internal, "Update recovery record disappeared during a mutation.")
	}
	change(record)
	return m.write(*record)
}

func (m *recoveryManager) read() (*recoveryRecord, error) {
	f, err := os.Open(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "Update recovery record is unreadable.")
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	var record recoveryRecord
	if err := decoder.Decode(&record); err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Internal, "Update recovery record is invalid.")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, pmuxerr.New(pmuxerr.JournalCorrupt, pmuxerr.Internal, "Update recovery record contains trailing data.")
	}
	if record.Version != recoveryVersion || (record.Component != update.Self && record.Component != update.Proxy) || record.Phase == "" {
		return nil, pmuxerr.New(pmuxerr.JournalCorrupt, pmuxerr.Internal, "Update recovery record has an unsupported schema.")
	}
	return &record, nil
}

func (m *recoveryManager) write(record recoveryRecord) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "Could not create private update recovery state.")
	}
	record.Version = recoveryVersion
	record.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(record)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Internal, "Could not encode update recovery state.")
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".pmux-update-state-")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "Could not stage update recovery state.")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write([]byte("\n")); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(m.path)); err != nil {
		return err
	}
	return nil
}

func (m *recoveryManager) clear() error {
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "Could not clear completed update recovery state.")
	}
	return syncDir(filepath.Dir(m.path))
}

func (m *recoveryManager) recover(ctx context.Context, e *Engine) error {
	record, err := m.read()
	if err != nil || record == nil {
		return err
	}
	if err := m.checkpoint(func(r *recoveryRecord) { r.Phase = "recovering" }); err != nil {
		return err
	}
	switch record.Component {
	case update.Proxy:
		err = m.recoverProxy(ctx, e, *record)
	case update.Self:
		err = m.recoverSelf(ctx, e, *record)
	}
	if err != nil {
		return err
	}
	return m.clear()
}

func (m *recoveryManager) recoverProxy(ctx context.Context, e *Engine, record recoveryRecord) error {
	if record.Workspace != "" && !filepath.IsAbs(record.Workspace) {
		return invalidRecoveryPath("workspace")
	}
	if record.PointerPath == "" {
		return removeRecoveryWorkspace(record.Workspace)
	}
	if !filepath.IsAbs(record.PointerPath) || !filepath.IsAbs(record.OldTarget) || (record.NewTarget != "" && !filepath.IsAbs(record.NewTarget)) {
		return invalidRecoveryPath("proxy pointer")
	}
	selected, err := e.pointerStore.Read(record.PointerPath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not inspect the interrupted proxy update pointer.")
	}
	needsLifecycle := selected == record.NewTarget || phaseHasServiceSideEffects(record.Phase)
	if selected == record.NewTarget {
		if e.service == nil {
			return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Cannot recover an interrupted proxy update without its service backend.")
		}
		_ = e.service.Stop(ctx, recoveryTimeout(record))
		if err := e.pointerStore.Swap(record.PointerPath, record.OldTarget); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not restore the previous proxy pointer.")
		}
		selected = record.OldTarget
	}
	if selected != record.OldTarget {
		return &pmuxerr.Error{Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Environment, Message: "Proxy current pointer changed during interrupted-update recovery.", Evidence: []string{record.PointerPath}}
	}
	if needsLifecycle {
		if e.service == nil || e.proxyVerifier == nil {
			return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Cannot verify the restored proxy after interrupted-update recovery.")
		}
		if err := e.service.Start(ctx); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "Could not start the restored proxy during interrupted-update recovery.")
		}
		if err := e.proxyVerifier.Health(ctx); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Restored proxy failed health verification.")
		}
		if err := e.proxyVerifier.Authenticated(ctx); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Restored proxy failed authenticated reachability verification.")
		}
		if !record.WasRunning {
			if err := e.service.Stop(ctx, recoveryTimeout(record)); err != nil {
				return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not restore the proxy's stopped state.")
			}
		}
	}
	if record.FinalDir != "" && record.FinalDir != record.OldTarget {
		if !filepath.IsAbs(record.FinalDir) {
			return invalidRecoveryPath("final version directory")
		}
		if err := os.RemoveAll(record.FinalDir); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not remove the interrupted proxy version.")
		}
	}
	return removeRecoveryWorkspace(record.Workspace)
}

func (m *recoveryManager) recoverSelf(ctx context.Context, e *Engine, record recoveryRecord) error {
	if record.CurrentPath == "" {
		return removeRecoveryWorkspace(record.Workspace)
	}
	if !filepath.IsAbs(record.CurrentPath) || !filepath.IsAbs(record.PreviousPath) {
		return invalidRecoveryPath("self-update executable")
	}
	if runtime.GOOS == "windows" {
		status, statusErr := readSelfUpdateStatus(record.CurrentPath + ".pmux-update-status.json")
		if statusErr == nil && status.State == "succeeded" {
			activeHash, err := hashHex(record.CurrentPath)
			if err == nil && activeHash == record.CandidateSHA256 && e.selfVerifier != nil {
				if err := e.selfVerifier.Postflight(ctx, record.CurrentPath, record.NewVersion); err == nil {
					return removeRecoveryWorkspace(record.Workspace)
				}
			}
		}
	}
	activeHash, err := hashHex(record.CurrentPath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not inspect the active PMux executable during recovery.")
	}
	if activeHash != record.CurrentSHA256 {
		previousHash, hashErr := hashHex(record.PreviousPath)
		if hashErr != nil || previousHash != record.CurrentSHA256 {
			return pmuxerr.Wrap(coalesce(hashErr, errors.New("retained executable fingerprint mismatch")), pmuxerr.InstallRollbackAttempted, pmuxerr.Internal, "Interrupted self-update cannot safely restore the previous executable.")
		}
		info, statErr := os.Stat(record.PreviousPath)
		if statErr != nil {
			return pmuxerr.Wrap(statErr, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Retained PMux executable is unreadable during recovery.")
		}
		if err := copyFileAtomic(record.PreviousPath, record.CurrentPath, info.Mode().Perm()); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not restore the previous PMux executable.")
		}
	}
	if e.selfVerifier == nil {
		return pmuxerr.New(pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Cannot verify the restored PMux executable.")
	}
	if err := e.selfVerifier.Postflight(ctx, record.CurrentPath, record.OldVersion); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Restored PMux executable failed version verification.")
	}
	return removeRecoveryWorkspace(record.Workspace)
}

func readSelfUpdateStatus(path string) (selfUpdateStatus, error) {
	var status selfUpdateStatus
	body, err := os.ReadFile(path)
	if err != nil {
		return status, err
	}
	err = json.Unmarshal(body, &status)
	return status, err
}

func hashHex(path string) (string, error) {
	hash, err := fileHash(path)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash[:]), nil
}

func removeRecoveryWorkspace(path string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return invalidRecoveryPath("workspace")
	}
	if err := os.RemoveAll(path); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.InstallRollbackAttempted, pmuxerr.Environment, "Could not remove interrupted update staging.")
	}
	return nil
}

func recoveryTimeout(record recoveryRecord) time.Duration {
	if record.StopTimeoutNS > 0 {
		return time.Duration(record.StopTimeoutNS)
	}
	return defaultStopTimeout
}

func phaseHasServiceSideEffects(phase string) bool {
	switch phase {
	case "stopping-service", "installing-version", "switching-pointer", "starting-service", "verifying", "recovering":
		return true
	default:
		return false
	}
}

func invalidRecoveryPath(name string) error {
	return &pmuxerr.Error{Code: pmuxerr.JournalCorrupt, Class: pmuxerr.Internal, Message: "Update recovery record contains an unsafe " + name + " path."}
}

func archiveDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r recoveryRecord) String() string {
	return fmt.Sprintf("%s update at %s", r.Component, r.Phase)
}
