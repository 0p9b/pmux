//go:build !windows

package installer

import (
	"errors"
	"os"
	"path/filepath"
)

func readCurrentPointer(current string) (string, bool, error) {
	target, err := os.Readlink(current)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(current), target)
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", false, err
	}
	return absolute, true, nil
}

func writeCurrentPointer(root, current, versionDir string) error {
	temporary, err := os.CreateTemp(root, ".current-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	relative, err := filepath.Rel(root, versionDir)
	if err != nil {
		return err
	}
	if err := os.Symlink(relative, temporaryPath); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := os.Rename(temporaryPath, current); err != nil {
		return err
	}
	committed = true
	return syncDir(root)
}

func removeCurrentPointer(current string) error {
	if err := os.Remove(current); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(current))
}
