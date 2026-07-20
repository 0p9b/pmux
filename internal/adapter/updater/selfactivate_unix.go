//go:build !windows

package updater

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func (e *Engine) activateSelf(ctx context.Context, result Result, current, candidate string, _ [32]byte, mode os.FileMode, currentVersion, nextVersion string) (Result, error) {
	if err := e.stage(StageActivate); err != nil {
		return result, stageError(StageActivate, err)
	}
	previous := current + ".pmux-previous"
	if err := copyFileAtomic(current, previous, mode); err != nil {
		return result, normalize(err, pmuxerr.InstallRollbackAttempted, "Could not retain the current PMux executable for rollback.")
	}
	if err := os.Rename(candidate, current); err != nil {
		return result, normalize(err, pmuxerr.InstallRollbackAttempted, "Could not activate the verified PMux executable; the current executable is unchanged.")
	}
	rollback := func(cause error) (Result, error) {
		rolledBack, rollbackErr := e.rollbackSelf(current, previous, currentVersion, cause)
		result.RolledBack = rolledBack
		return result, rollbackErr
	}
	if err := syncDir(filepath.Dir(current)); err != nil {
		return rollback(fmt.Errorf("sync activation directory: %w", err))
	}
	if err := e.stage(StagePostflight); err != nil {
		return rollback(err)
	}
	if err := e.selfVerifier.Postflight(ctx, current, nextVersion); err != nil {
		return rollback(fmt.Errorf("post-update version verification: %w", err))
	}
	result.Changed = true
	return result, nil
}
