package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/domain/update"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const defaultStopTimeout = 15 * time.Second

// Engine performs updates only through its explicit methods. New has no I/O side effects.
type Engine struct {
	source         Source
	service        Service
	proxyVerifier  ProxyVerifier
	selfVerifier   SelfVerifier
	pointerStore   PointerStore
	stageHook      func(Stage) error
	recoveryPath   string
	recoveryLocker MutationLocker
}

func New(source Source, service Service, proxyVerifier ProxyVerifier, selfVerifier SelfVerifier, options ...Option) *Engine {
	e := &Engine{source: source, service: service, proxyVerifier: proxyVerifier, selfVerifier: selfVerifier, pointerStore: nativePointerStore{}}
	for _, option := range options {
		option(e)
	}
	if e.pointerStore == nil {
		e.pointerStore = nativePointerStore{}
	}
	return e
}

// Check is an explicit metadata-only release lookup. It never downloads an asset.
func (e *Engine) Check(ctx context.Context, req CheckRequest) (update.Release, error) {
	if e.source == nil {
		return update.Release{}, missingSource()
	}
	if req.Component != update.Self && req.Component != update.Proxy {
		return update.Release{}, &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.User, Message: "Unknown update component."}
	}
	if err := e.stage(StageResolve); err != nil {
		return update.Release{}, stageError(StageResolve, err)
	}
	release, err := e.source.Resolve(ctx, req.Component, req.Version)
	if err != nil {
		return update.Release{}, normalize(err, pmuxerr.InstallReleaseLookupFailed, "Release lookup failed.")
	}
	if err := validateRelease(release, req.Component); err != nil {
		return update.Release{}, err
	}
	return update.Release{Component: req.Component, Current: req.CurrentVersion, Available: release.Version, URL: release.ArchiveURL, CheckedAt: time.Now().UTC()}, nil
}

func (e *Engine) UpdateSelf(ctx context.Context, req SelfRequest) (Result, error) {
	result := Result{Component: update.Self, PreviousVersion: req.CurrentVersion}
	if err := requireManaged(update.Self, req.Ownership); err != nil {
		return result, err
	}
	if e.source == nil {
		return result, missingSource()
	}
	if e.selfVerifier == nil {
		return result, &pmuxerr.Error{Code: pmuxerr.InstallUnsupportedTarget, Class: pmuxerr.Environment, Message: "Self-update verifier is unavailable; the existing executable was not changed."}
	}
	current, err := filepath.Abs(req.ExecutablePath)
	if err != nil || req.ExecutablePath == "" {
		return result, pmuxerr.Wrap(coalesce(err, errors.New("empty executable path")), pmuxerr.ConfigValidationFailed, pmuxerr.User, "Self-update requires an absolute executable path.")
	}
	recovery, err := e.recoveryFor(update.Self, filepath.Dir(current))
	if err != nil {
		return result, err
	}
	var out Result
	err = recovery.withMutation(ctx, "update.self", func() error {
		if recoverErr := recovery.recover(ctx, e); recoverErr != nil {
			return recoverErr
		}
		info, statErr := os.Stat(current)
		if statErr != nil {
			return pmuxerr.Wrap(statErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Current PMux executable is not readable.")
		}
		if !info.Mode().IsRegular() {
			return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.Environment, Message: "Current PMux executable is not a regular file."}
		}
		originalHash, hashErr := fileHash(current)
		if hashErr != nil {
			return pmuxerr.Wrap(hashErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Current PMux executable could not be fingerprinted.")
		}
		if beginErr := recovery.begin(update.Self, req.CurrentVersion); beginErr != nil {
			return beginErr
		}
		open := true
		defer func() {
			if open {
				_ = recovery.recover(context.Background(), e)
			}
		}()
		release, workspace, candidate, prepareErr := e.prepare(ctx, update.Self, req.CurrentVersion, req.Version, req.Target, filepath.Dir(current))
		if workspace != "" {
			defer os.RemoveAll(workspace)
		}
		if prepareErr != nil {
			return prepareErr
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) {
			r.Phase = "prepared"
			r.Workspace = workspace
			r.CandidatePath = candidate
			r.CurrentPath = current
			r.PreviousPath = current + ".pmux-previous"
			r.OldVersion = req.CurrentVersion
			r.NewVersion = release.Version
		}); checkpointErr != nil {
			return checkpointErr
		}
		result.Version = release.Version
		if release.Version == req.CurrentVersion {
			open = false
			_ = recovery.clear()
			out = result
			return nil
		}
		if stageErr := e.stage(StagePreflight); stageErr != nil {
			return stageError(StagePreflight, stageErr)
		}
		if preflightErr := e.selfVerifier.Preflight(ctx, candidate, release.Version); preflightErr != nil {
			return normalize(preflightErr, pmuxerr.InstallIntegrityFailed, "The downloaded PMux executable failed version verification.")
		}
		currentHash, hashErr := fileHash(current)
		if hashErr != nil || currentHash != originalHash {
			if hashErr == nil {
				hashErr = errors.New("current executable fingerprint changed")
			}
			return pmuxerr.Wrap(hashErr, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "PMux executable changed while the update was being prepared; activation was refused.")
		}
		if chmodErr := os.Chmod(candidate, info.Mode().Perm()|0o100); chmodErr != nil {
			return normalize(chmodErr, pmuxerr.InstallIntegrityFailed, "Could not prepare executable permissions before activation.")
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) {
			r.Phase = "activating"
			r.CurrentSHA256 = hex.EncodeToString(currentHash[:])
			if digest, digestErr := hashHex(candidate); digestErr == nil {
				r.CandidateSHA256 = digest
			}
		}); checkpointErr != nil {
			return checkpointErr
		}
		activated, activateErr := e.activateSelf(ctx, result, current, candidate, currentHash, info.Mode().Perm(), req.CurrentVersion, release.Version)
		if activateErr != nil {
			out = activated
			open = false
			_ = recovery.clear()
			return activateErr
		}
		open = false
		if clearErr := recovery.clear(); clearErr != nil {
			return clearErr
		}
		out = activated
		return nil
	})
	if err != nil {
		if out.Component == "" {
			out = result
		}
		return out, err
	}
	return out, nil
}

func (e *Engine) UpdateProxy(ctx context.Context, req ProxyRequest) (Result, error) {
	result := Result{Component: update.Proxy, PreviousVersion: req.CurrentVersion}
	if err := requireManaged(update.Proxy, req.Ownership); err != nil {
		return result, err
	}
	if e.source == nil {
		return result, missingSource()
	}
	if e.service == nil || e.proxyVerifier == nil {
		return result, &pmuxerr.Error{Code: pmuxerr.ServiceBackendUnavailable, Class: pmuxerr.Environment, Message: "Proxy update requires a service backend and local verification client."}
	}
	versionsDir, err := filepath.Abs(req.VersionsDir)
	if err != nil || req.VersionsDir == "" {
		return result, pmuxerr.Wrap(coalesce(err, errors.New("empty versions directory")), pmuxerr.ConfigValidationFailed, pmuxerr.User, "Proxy versions directory is invalid.")
	}
	pointer, err := filepath.Abs(req.CurrentPointer)
	if err != nil || req.CurrentPointer == "" {
		return result, pmuxerr.Wrap(coalesce(err, errors.New("empty current pointer")), pmuxerr.ConfigValidationFailed, pmuxerr.User, "Proxy current pointer is invalid.")
	}
	recovery, err := e.recoveryFor(update.Proxy, versionsDir)
	if err != nil {
		return result, err
	}
	var out Result
	err = recovery.withMutation(ctx, "update.proxy", func() error {
		if recoverErr := recovery.recover(ctx, e); recoverErr != nil {
			return recoverErr
		}
		oldTarget, readErr := e.pointerStore.Read(pointer)
		if readErr != nil {
			return pmuxerr.Wrap(readErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Managed proxy current pointer is unreadable.")
		}
		if beginErr := recovery.begin(update.Proxy, req.CurrentVersion); beginErr != nil {
			return beginErr
		}
		open := true
		defer func() {
			if open {
				_ = recovery.recover(context.Background(), e)
			}
		}()
		release, workspace, candidate, prepareErr := e.prepare(ctx, update.Proxy, req.CurrentVersion, req.Version, req.Target, versionsDir)
		if workspace != "" {
			defer os.RemoveAll(workspace)
		}
		if prepareErr != nil {
			return prepareErr
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) {
			r.Phase = "prepared"
			r.Workspace = workspace
			r.CandidatePath = candidate
			r.PointerPath = pointer
			r.OldTarget = oldTarget
			r.OldVersion = req.CurrentVersion
			r.NewVersion = release.Version
		}); checkpointErr != nil {
			return checkpointErr
		}
		result.Version = release.Version
		if release.Version == req.CurrentVersion {
			open = false
			_ = recovery.clear()
			out = result
			return nil
		}

		status, statusErr := e.service.Status(ctx)
		if statusErr != nil {
			return normalize(statusErr, pmuxerr.ServiceBackendUnavailable, "Could not read the proxy service state before update.")
		}
		wasRunning := status.State == service.ServiceRunning || status.State == service.ServiceStarting
		stopTimeout := req.StopTimeout
		if stopTimeout <= 0 {
			stopTimeout = defaultStopTimeout
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) {
			r.WasRunning = wasRunning
			r.StopTimeoutNS = int64(stopTimeout)
		}); checkpointErr != nil {
			return checkpointErr
		}
		if wasRunning {
			if stageErr := e.stage(StageStopService); stageErr != nil {
				return stageError(StageStopService, stageErr)
			}
			if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) { r.Phase = "stopping-service" }); checkpointErr != nil {
				return checkpointErr
			}
			if stopErr := e.service.Stop(ctx, stopTimeout); stopErr != nil {
				return normalize(stopErr, pmuxerr.ServiceStartFailed, "Could not stop CLIProxyAPI before update; activation was refused.")
			}
		}

		finalDir := filepath.Join(versionsDir, release.Version)
		if _, existsErr := os.Lstat(finalDir); existsErr == nil {
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return &pmuxerr.Error{Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Environment, Message: "The target proxy version directory already exists; activation was refused.", Evidence: []string{finalDir}}
		} else if !errors.Is(existsErr, os.ErrNotExist) {
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return pmuxerr.Wrap(existsErr, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "Could not inspect the target proxy version directory.")
		}
		if chmodErr := os.Chmod(candidate, 0o700); chmodErr != nil {
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return normalize(chmodErr, pmuxerr.InstallIntegrityFailed, "Could not prepare proxy executable permissions.")
		}
		if stageErr := e.stage(StageInstallVersion); stageErr != nil {
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return stageError(StageInstallVersion, stageErr)
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) {
			r.Phase = "installing-version"
			r.NewTarget = finalDir
			r.FinalDir = finalDir
		}); checkpointErr != nil {
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return checkpointErr
		}
		// The whole private extraction directory becomes the immutable version slot.
		extractedDir := filepath.Dir(candidate)
		if renameErr := os.Rename(extractedDir, finalDir); renameErr != nil {
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return normalize(renameErr, pmuxerr.InstallRollbackAttempted, "Could not install the verified proxy version.")
		}
		if stageErr := e.stage(StageSwitchPointer); stageErr != nil {
			_ = os.RemoveAll(finalDir)
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return stageError(StageSwitchPointer, stageErr)
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) { r.Phase = "switching-pointer" }); checkpointErr != nil {
			_ = os.RemoveAll(finalDir)
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return checkpointErr
		}
		if swapErr := e.pointerStore.Swap(pointer, finalDir); swapErr != nil {
			_ = os.RemoveAll(finalDir)
			if wasRunning {
				_ = e.service.Start(ctx)
			}
			return normalize(swapErr, pmuxerr.InstallRollbackAttempted, "Could not select the verified proxy version; the prior pointer was retained.")
		}
		failure := func(cause error) error {
			rolledBack, rollbackErr := e.rollbackProxy(ctx, pointer, oldTarget, finalDir, wasRunning, stopTimeout, cause)
			result.RolledBack = rolledBack
			out = result
			return rollbackErr
		}
		if stageErr := e.stage(StageStartService); stageErr != nil {
			return failure(stageErr)
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) { r.Phase = "starting-service" }); checkpointErr != nil {
			return failure(checkpointErr)
		}
		if startErr := e.service.Start(ctx); startErr != nil {
			return failure(fmt.Errorf("start updated proxy: %w", startErr))
		}
		if stageErr := e.stage(StageHealth); stageErr != nil {
			return failure(stageErr)
		}
		if checkpointErr := recovery.checkpoint(func(r *recoveryRecord) { r.Phase = "verifying" }); checkpointErr != nil {
			return failure(checkpointErr)
		}
		if healthErr := e.proxyVerifier.Health(ctx); healthErr != nil {
			return failure(fmt.Errorf("updated proxy health: %w", healthErr))
		}
		if stageErr := e.stage(StageAuthenticate); stageErr != nil {
			return failure(stageErr)
		}
		if authErr := e.proxyVerifier.Authenticated(ctx); authErr != nil {
			return failure(fmt.Errorf("updated proxy authentication: %w", authErr))
		}
		if stageErr := e.stage(StageModels); stageErr != nil {
			return failure(stageErr)
		}
		models, modelsErr := e.proxyVerifier.Models(ctx)
		if modelsErr != nil {
			return failure(fmt.Errorf("updated proxy model endpoint: %w", modelsErr))
		}
		if len(models) == 0 {
			result.Warnings = append(result.Warnings, "No effective credentials; model catalog is empty.")
		}
		if !wasRunning {
			if stopErr := e.service.Stop(ctx, stopTimeout); stopErr != nil {
				return failure(fmt.Errorf("restore stopped service state: %w", stopErr))
			}
		}
		result.Changed = true
		open = false
		if clearErr := recovery.clear(); clearErr != nil {
			return clearErr
		}
		out = result
		return nil
	})
	if err != nil {
		if out.Component == "" {
			out = result
		}
		return out, err
	}
	return out, nil
}

func (e *Engine) prepare(ctx context.Context, component update.Component, currentVersion, version string, target Target, parent string) (Release, string, string, error) {
	if target.GOOS == "" || target.Arch == "" { target = NativeTarget() }
	if err := os.MkdirAll(parent, 0o700); err != nil { return Release{}, "", "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not create update staging parent.") }
	if err := e.stage(StageResolve); err != nil { return Release{}, "", "", stageError(StageResolve, err) }
	release, err := e.source.Resolve(ctx, component, version)
	if err != nil { return Release{}, "", "", normalize(err, pmuxerr.InstallReleaseLookupFailed, "Release lookup failed.") }
	if err := validateRelease(release, component); err != nil { return Release{}, "", "", err }
	if release.Version == currentVersion { return release, "", "", nil }
	workspace, err := os.MkdirTemp(parent, ".pmux-update-")
	if err != nil { return Release{}, "", "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not create private update staging.") }
	if err := os.Chmod(workspace, 0o700); err != nil { _ = os.RemoveAll(workspace); return Release{}, "", "", pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "Could not secure update staging.") }
	archivePath := filepath.Join(workspace, release.ArchiveName)
	checksumsPath := filepath.Join(workspace, "checksums.txt")
	if err := e.stage(StageDownloadArchive); err != nil { return release, workspace, "", stageError(StageDownloadArchive, err) }
	if err := e.source.Download(ctx, release.ArchiveURL, archivePath); err != nil { return release, workspace, "", normalize(err, pmuxerr.InstallDownloadFailed, "Release archive download failed.") }
	if err := e.stage(StageDownloadChecksums); err != nil { return release, workspace, "", stageError(StageDownloadChecksums, err) }
	if err := e.source.Download(ctx, release.ChecksumsURL, checksumsPath); err != nil { return release, workspace, "", normalize(err, pmuxerr.InstallDownloadFailed, "Release checksum download failed.") }
	if err := e.stage(StageVerifyChecksum); err != nil { return release, workspace, "", stageError(StageVerifyChecksum, err) }
	if err := verifyArchiveChecksum(archivePath, checksumsPath, release.ArchiveName); err != nil { return release, workspace, "", err }
	if err := e.stage(StageExtract); err != nil { return release, workspace, "", stageError(StageExtract, err) }
	candidate, err := extractExecutable(archivePath, filepath.Join(workspace, "extracted"), release.ExecutableName)
	if err != nil { return release, workspace, "", err }
	if err := e.stage(StageVerifyExecutable); err != nil { return release, workspace, "", stageError(StageVerifyExecutable, err) }
	if err := verifyExecutable(candidate, target); err != nil { return release, workspace, "", err }
	return release, workspace, candidate, nil
}

func (e *Engine) rollbackSelf(current, previous, expectedVersion string, cause error) (bool, error) {
	info, statErr := os.Stat(previous)
	if statErr != nil { return false, rollbackError(cause, fmt.Errorf("stat retained executable: %w", statErr)) }
	tmp := current + ".rollback-tmp"
	_ = os.Remove(tmp)
	if err := copyFile(previous, tmp, info.Mode().Perm()); err != nil { return false, rollbackError(cause, err) }
	if err := os.Rename(tmp, current); err != nil { _ = os.Remove(tmp); return false, rollbackError(cause, err) }
	if err := syncDir(filepath.Dir(current)); err != nil { return false, rollbackError(cause, err) }
	if err := e.selfVerifier.Postflight(context.Background(), current, expectedVersion); err != nil { return false, rollbackError(cause, fmt.Errorf("restored executable verification: %w", err)) }
	return true, &pmuxerr.Error{Code: pmuxerr.InstallRollbackAttempted, Class: pmuxerr.Upstream, Message: "Updated PMux failed verification and the previous executable was restored.", Cause: cause}
}

func (e *Engine) rollbackProxy(ctx context.Context, pointer, oldTarget, finalDir string, wasRunning bool, timeout time.Duration, cause error) (bool, error) {
	var rollbackErrs []error
	if err := e.service.Stop(ctx, timeout); err != nil { rollbackErrs = append(rollbackErrs, fmt.Errorf("stop failed version: %w", err)) }
	if err := e.pointerStore.Swap(pointer, oldTarget); err != nil { rollbackErrs = append(rollbackErrs, fmt.Errorf("restore pointer: %w", err)) } else {
		if err := e.service.Start(ctx); err != nil { rollbackErrs = append(rollbackErrs, fmt.Errorf("start restored proxy: %w", err)) } else {
			if err := e.proxyVerifier.Health(ctx); err != nil { rollbackErrs = append(rollbackErrs, fmt.Errorf("restored proxy health: %w", err)) }
			if err := e.proxyVerifier.Authenticated(ctx); err != nil { rollbackErrs = append(rollbackErrs, fmt.Errorf("restored proxy authentication: %w", err)) }
		}
		if !wasRunning {
			if err := e.service.Stop(ctx, timeout); err != nil { rollbackErrs = append(rollbackErrs, fmt.Errorf("restore stopped state: %w", err)) }
		}
	}
	if len(rollbackErrs) == 0 { _ = os.RemoveAll(finalDir) }
	if len(rollbackErrs) > 0 { return false, rollbackError(cause, errors.Join(rollbackErrs...)) }
	return true, &pmuxerr.Error{Code: pmuxerr.InstallRollbackAttempted, Class: pmuxerr.Upstream, Message: "Updated CLIProxyAPI failed verification and was rolled back to the previous version.", Cause: cause}
}

func requireManaged(component update.Component, ownership Ownership) error {
	if ownership == OwnershipManaged { return nil }
	message := "This component is not PMux-managed; update it with its owning installation method."
	if ownership == OwnershipAdopted && component == update.Proxy { message = "CLIProxyAPI is adopted, not PMux-managed; update it with its owning installation method." }
	if ownership == OwnershipPackageManaged && component == update.Self { message = "PMux was installed by a package manager; update it with the package manager." }
	return &pmuxerr.Error{Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.User, Message: message}
}

func validateRelease(release Release, component update.Component) error {
	if release.Component != component || release.Version == "" || release.ArchiveName == "" || filepath.Base(release.ArchiveName) != release.ArchiveName || release.ArchiveURL == "" || release.ChecksumsURL == "" || release.ExecutableName == "" || filepath.Base(release.ExecutableName) != release.ExecutableName {
		return &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.Upstream, Message: "Release metadata is incomplete or unsafe."}
	}
	return nil
}

func (e *Engine) stage(stage Stage) error {
	if e.stageHook == nil { return nil }
	return e.stageHook(stage)
}

func stageError(stage Stage, err error) error {
	code, class := pmuxerr.InstallRollbackAttempted, pmuxerr.Environment
	switch stage {
	case StageResolve: code = pmuxerr.InstallReleaseLookupFailed
	case StageDownloadArchive, StageDownloadChecksums: code = pmuxerr.InstallDownloadFailed
	case StageVerifyChecksum, StageExtract, StageVerifyExecutable, StagePreflight: code = pmuxerr.InstallIntegrityFailed; class = pmuxerr.Upstream
	case StageStopService, StageStartService: code = pmuxerr.ServiceStartFailed
	case StageHealth: code = pmuxerr.ServiceHealthDeadline
	case StageAuthenticate: code = pmuxerr.ManagementAuthRejected
	case StageModels: code = pmuxerr.ManagementUnreachable
	}
	return pmuxerr.Wrap(err, code, class, "Update failed at stage "+string(stage)+".")
}

func normalize(err error, code, message string) error {
	if err == nil { return nil }
	var pe *pmuxerr.Error
	if errors.As(err, &pe) { return err }
	return pmuxerr.Wrap(err, code, pmuxerr.Environment, message)
}

func rollbackError(cause, rollback error) error {
	return &pmuxerr.Error{Code: pmuxerr.InstallRollbackAttempted, Class: pmuxerr.Internal, Message: "Update failed and rollback could not be verified.", Evidence: []string{"manual recovery is required"}, Cause: errors.Join(cause, rollback)}
}

func missingSource() error { return &pmuxerr.Error{Code: pmuxerr.InstallReleaseLookupFailed, Class: pmuxerr.Internal, Message: "Release source is unavailable."} }
func coalesce(err, fallback error) error { if err != nil { return err }; return fallback }

func fileHash(path string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil { return out, err }
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil { return out, err }
	copy(out[:], h.Sum(nil))
	return out, nil
}

func copyFileAtomic(source, destination string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".pmux-copy-")
	if err != nil { return err }
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)
	defer os.Remove(tmpName)
	if err := copyFile(source, tmpName, mode); err != nil { return err }
	if err := os.Rename(tmpName, destination); err != nil { return err }
	return syncDir(filepath.Dir(destination))
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil { return err }
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil { return err }
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil { return copyErr }
	if syncErr != nil { return syncErr }
	return closeErr
}

func atomicSymlink(target, pointer string) error {
	tmp, err := os.CreateTemp(filepath.Dir(pointer), ".pmux-current-")
	if err != nil { return err }
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)
	defer os.Remove(tmpName)
	if err := os.Symlink(target, tmpName); err != nil { return err }
	if err := os.Rename(tmpName, pointer); err != nil { return err }
	return syncDir(filepath.Dir(pointer))
}
