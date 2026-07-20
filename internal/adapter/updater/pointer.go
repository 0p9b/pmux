package updater

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type nativePointerStore struct{}

func (nativePointerStore) Read(pointer string) (string, error) {
	if runtime.GOOS != "windows" {
		return os.Readlink(pointer)
	}
	body, err := os.ReadFile(pointer)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(body))
	if target == "" || !filepath.IsAbs(target) {
		return "", errors.New("managed pointer file does not contain an absolute target")
	}
	return target, nil
}

func (nativePointerStore) Swap(pointer, target string) error {
	if runtime.GOOS != "windows" {
		return atomicSymlink(target, pointer)
	}
	if !filepath.IsAbs(target) {
		return errors.New("managed pointer target must be absolute")
	}
	tmp, err := os.CreateTemp(filepath.Dir(pointer), ".pmux-current-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(target + "\n"); err != nil {
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
	if err := os.Rename(tmpName, pointer); err != nil {
		return err
	}
	return syncDir(filepath.Dir(pointer))
}
