package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	adapterfs "github.com/0p9b/pmux/internal/adapter/fs"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const selfUpdateHelperMarker = "__pmux-self-update-helper"

type selfUpdatePlan struct {
	ParentPID       int    `json:"parent_pid"`
	ActivePath      string `json:"active_path"`
	ReplacementPath string `json:"replacement_path"`
	HelperPath      string `json:"helper_path"`
	PreviousPath    string `json:"previous_path"`
	StatusPath      string `json:"status_path"`
	ActiveSHA256    string `json:"active_sha256"`
	ReplacementSHA256 string `json:"replacement_sha256"`
	HelperSHA256    string `json:"helper_sha256"`
	CurrentVersion  string `json:"current_version"`
	NextVersion     string `json:"next_version"`
}

type selfUpdateStatus struct {
	State       string    `json:"state"`
	Version     string    `json:"version,omitempty"`
	RolledBack  bool      `json:"rolled_back"`
	Message     string    `json:"message"`
	CompletedAt time.Time `json:"completed_at"`
}

type selfHelperOps interface {
	WaitParent(context.Context, int) error
	Hash(string) ([sha256.Size]byte, error)
	MoveReplace(string, string) error
	Remove(string) error
	VerifyVersion(context.Context, string, string) error
	WriteStatus(string, selfUpdateStatus) error
	Cleanup(selfUpdatePlan)
}

func IsSelfUpdateHelperInvocation(args []string) bool {
	return len(args) == 2 && args[0] == selfUpdateHelperMarker && args[1] != ""
}

// RunSelfUpdateHelper executes the private detached helper path before Cobra is
// constructed. The marker is deliberately not part of PMux's public grammar.
func RunSelfUpdateHelper(ctx context.Context, args []string) error {
	if !IsSelfUpdateHelperInvocation(args) {
		return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.User, Message: "Invalid private self-update helper invocation."}
	}
	payload, err := os.ReadFile(args[1])
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Private self-update plan could not be read.")
	}
	var plan selfUpdatePlan
	if err := json.Unmarshal(payload, &plan); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "Private self-update plan is invalid.")
	}
	if err := validateSelfUpdatePlan(plan); err != nil {
		return err
	}
	return runSelfUpdateHelper(ctx, plan, newPlatformSelfHelperOps())
}

func runSelfUpdateHelper(ctx context.Context, plan selfUpdatePlan, ops selfHelperOps) error {
	status := selfUpdateStatus{State: "failed", Version: plan.NextVersion, Message: "Self-update did not complete."}
	finish := func(result error) error {
		status.CompletedAt = time.Now().UTC()
		writeErr := ops.WriteStatus(plan.StatusPath, status)
		ops.Cleanup(plan)
		return errors.Join(result, writeErr)
	}
	if err := ops.WaitParent(ctx, plan.ParentPID); err != nil {
		status.State = "interrupted"
		status.Message = "Self-update was interrupted before activation."
		return finish(err)
	}
	if err := ctx.Err(); err != nil {
		status.State = "interrupted"
		status.Message = "Self-update was interrupted before activation."
		return finish(err)
	}
	if err := verifyHelperFingerprint(ops, plan.ActivePath, plan.ActiveSHA256, "active executable"); err != nil {
		status.State = "conflict"
		status.Message = "The active PMux executable changed before activation; the update was refused."
		return finish(err)
	}
	if err := verifyHelperFingerprint(ops, plan.ReplacementPath, plan.ReplacementSHA256, "staged replacement"); err != nil {
		status.State = "conflict"
		status.Message = "The staged PMux replacement changed before activation; the update was refused."
		return finish(err)
	}
	if err := verifyHelperFingerprint(ops, plan.HelperPath, plan.HelperSHA256, "detached helper"); err != nil {
		status.State = "conflict"
		status.Message = "The detached PMux helper changed before activation; the update was refused."
		return finish(err)
	}
	if err := ops.MoveReplace(plan.ActivePath, plan.PreviousPath); err != nil {
		return finish(fmt.Errorf("retain active executable: %w", err))
	}
	rollback := func(cause error) error {
		_ = ops.Remove(plan.ActivePath)
		if err := ops.MoveReplace(plan.PreviousPath, plan.ActivePath); err != nil {
			status.Message = "Self-update failed and the prior executable could not be restored."
			return finish(errors.Join(cause, fmt.Errorf("restore prior executable: %w", err)))
		}
		verifyCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := ops.VerifyVersion(verifyCtx, plan.ActivePath, plan.CurrentVersion); err != nil {
			status.Message = "Self-update failed and rollback verification failed."
			return finish(errors.Join(cause, fmt.Errorf("verify restored executable: %w", err)))
		}
		status.State = "rolled_back"
		status.RolledBack = true
		status.Version = plan.CurrentVersion
		status.Message = "Self-update postflight failed; the prior PMux executable was restored."
		return finish(cause)
	}
	if err := ctx.Err(); err != nil {
		status.State = "interrupted"
		status.Message = "Self-update was interrupted during activation; rollback was attempted."
		return rollback(err)
	}
	if err := ops.MoveReplace(plan.ReplacementPath, plan.ActivePath); err != nil {
		return rollback(fmt.Errorf("activate staged executable: %w", err))
	}
	postflightCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ops.VerifyVersion(postflightCtx, plan.ActivePath, plan.NextVersion); err != nil {
		return rollback(fmt.Errorf("post-update version verification: %w", err))
	}
	status.State = "succeeded"
	status.Message = "Self-update completed successfully."
	return finish(nil)
}

func validateSelfUpdatePlan(plan selfUpdatePlan) error {
	paths := []string{plan.ActivePath, plan.ReplacementPath, plan.HelperPath, plan.PreviousPath, plan.StatusPath}
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.Environment, Message: "Private self-update plan contains a non-absolute path."}
		}
	}
	if plan.ParentPID <= 0 || plan.CurrentVersion == "" || plan.NextVersion == "" {
		return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.Environment, Message: "Private self-update plan is incomplete."}
	}
	for left := range paths {
		for _, other := range paths[left+1:] {
			if strings.EqualFold(filepath.Clean(paths[left]), filepath.Clean(other)) {
				return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.Environment, Message: "Private self-update plan contains overlapping paths."}
			}
		}
	}
	for _, value := range []string{plan.ActiveSHA256, plan.ReplacementSHA256, plan.HelperSHA256} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.Environment, Message: "Private self-update plan contains an invalid fingerprint."}
		}
	}
	return nil
}

func verifyHelperFingerprint(ops selfHelperOps, path, expected, label string) error {
	observed, err := ops.Hash(path)
	if err != nil {
		return fmt.Errorf("fingerprint %s: %w", label, err)
	}
	if hex.EncodeToString(observed[:]) != expected {
		return fmt.Errorf("%s fingerprint changed", label)
	}
	return nil
}

func writeSelfUpdatePlan(path string, plan selfUpdatePlan) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return adapterfs.AtomicWritePrivate(path, append(payload, '\n'))
}

func writeSelfUpdateStatus(path string, status selfUpdateStatus) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return adapterfs.AtomicWritePrivate(path, append(payload, '\n'))
}
