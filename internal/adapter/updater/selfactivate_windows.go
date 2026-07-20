//go:build windows

package updater

import (
	"context"
	"encoding/json"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	adapterfs "github.com/0p9b/pmux/internal/adapter/fs"
	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"golang.org/x/sys/windows"
)

func writeSelfUpdatePlan(path string, plan selfUpdatePlan) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return adapterfs.AtomicWritePrivate(path, append(payload, '\n'))
}

func (e *Engine) activateSelf(_ context.Context, result Result, current, candidate string, activeHash [32]byte, mode os.FileMode, currentVersion, nextVersion string) (Result, error) {
	if err := e.stage(StageActivate); err != nil {
		return result, stageError(StageActivate, err)
	}
	stageDir, err := os.MkdirTemp(filepath.Dir(current), ".pmux-self-update-*")
	if err != nil {
		return result, normalize(err, pmuxerr.InstallRollbackAttempted, "Could not create private Windows self-update staging.")
	}
	launched := false
	defer func() {
		if !launched {
			_ = os.RemoveAll(stageDir)
		}
	}()
	platform, err := adapterplatform.New()
	if err != nil {
		return result, err
	}
	if err := platform.SecurePermissions(stageDir, true); err != nil {
		return result, err
	}
	replacement := filepath.Join(stageDir, "replacement.exe")
	helper := filepath.Join(stageDir, "helper.exe")
	planPath := filepath.Join(stageDir, "plan.json")
	for _, destination := range []string{replacement, helper} {
		if err := copyFile(candidate, destination, mode|0o100); err != nil {
			return result, normalize(err, pmuxerr.InstallRollbackAttempted, "Could not stage the verified Windows self-update executable.")
		}
		if err := platform.SecurePermissions(destination, false); err != nil {
			return result, err
		}
	}
	replacementHash, err := fileHash(replacement)
	if err != nil {
		return result, normalize(err, pmuxerr.InstallIntegrityFailed, "Could not fingerprint the staged Windows replacement.")
	}
	helperHash, err := fileHash(helper)
	if err != nil {
		return result, normalize(err, pmuxerr.InstallIntegrityFailed, "Could not fingerprint the detached Windows update helper.")
	}
	plan := selfUpdatePlan{
		ParentPID: os.Getpid(), ActivePath: current, ReplacementPath: replacement,
		HelperPath: helper, PreviousPath: current + ".pmux-previous",
		StatusPath:        current + ".pmux-update-status.json",
		ActiveSHA256:      hex.EncodeToString(activeHash[:]),
		ReplacementSHA256: hex.EncodeToString(replacementHash[:]),
		HelperSHA256:      hex.EncodeToString(helperHash[:]),
		CurrentVersion:    currentVersion, NextVersion: nextVersion,
	}
	if err := writeSelfUpdatePlan(planPath, plan); err != nil {
		return result, normalize(err, pmuxerr.InstallRollbackAttempted, "Could not write the private Windows self-update plan.")
	}
	if err := platform.SecurePermissions(planPath, false); err != nil {
		return result, err
	}
	if err := launchDetachedSelfHelper(helper, planPath); err != nil {
		return result, normalize(err, pmuxerr.InstallRollbackAttempted, "Could not start the detached Windows self-update helper; the current executable is unchanged.")
	}
	launched = true
	result.Changed = true
	result.Warnings = append(result.Warnings, "Windows activation will complete after this PMux process exits; status is written to the private self-update handoff file.")
	return result, nil
}

func launchDetachedSelfHelper(helper, planPath string) error {
	cmd := exec.Command(helper, selfUpdateHelperMarker, planPath)
	cmd.Env = []string{}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
