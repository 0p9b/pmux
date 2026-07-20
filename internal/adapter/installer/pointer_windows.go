//go:build windows

package installer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows uses an atomically replaced pointer file. Native services resolve this
// pointer through PMux state, avoiding symlink privileges for ordinary users.
func readCurrentPointer(current string) (string, bool, error) {
	body, err := os.ReadFile(current)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	target := strings.TrimSpace(string(body))
	if target == "" || !filepath.IsAbs(target) {
		return "", false, errors.New("current pointer is empty or not absolute")
	}
	return filepath.Clean(target), true, nil
}

func writeCurrentPointer(root, current, versionDir string) error {
	temporary, err := os.CreateTemp(root, ".current-")
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
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.WriteString(filepath.Clean(versionDir) + "\r\n"); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(current)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
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
