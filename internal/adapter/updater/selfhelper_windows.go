//go:build windows

package updater

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"golang.org/x/sys/windows"
)

type windowsSelfHelperOps struct{}

func newPlatformSelfHelperOps() selfHelperOps { return windowsSelfHelperOps{} }

func (windowsSelfHelperOps) WaitParent(ctx context.Context, pid int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		result, err := windows.WaitForSingleObject(handle, 100)
		if err != nil {
			return err
		}
		switch result {
		case windows.WAIT_OBJECT_0:
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			return errors.New("unexpected parent wait result")
		}
	}
}

func (windowsSelfHelperOps) Hash(path string) ([sha256.Size]byte, error) {
	return fileHash(path)
}

func (windowsSelfHelperOps) MoveReplace(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func (windowsSelfHelperOps) Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (windowsSelfHelperOps) VerifyVersion(ctx context.Context, path, version string) error {
	return verifyCommandVersion(ctx, path, version)
}
func (windowsSelfHelperOps) WriteStatus(path string, status selfUpdateStatus) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	platform, err := adapterplatform.New()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pmux-update-status-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := platform.SecurePermissions(name, false); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return windowsSelfHelperOps{}.MoveReplace(name, path)
}

func (windowsSelfHelperOps) Cleanup(plan selfUpdatePlan) {
	_ = os.Remove(filepath.Join(filepath.Dir(plan.HelperPath), "plan.json"))
	_ = os.Remove(plan.ReplacementPath)
	_ = scheduleDeleteOnReboot(plan.HelperPath)
	_ = scheduleDeleteOnReboot(filepath.Dir(plan.HelperPath))
}

func scheduleDeleteOnReboot(path string) error {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(value, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

