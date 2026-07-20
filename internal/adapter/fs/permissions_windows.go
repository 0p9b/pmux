//go:build windows

package fs

import adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"

func protectPrivatePath(path string, isDir bool) error {
	platform, err := adapterplatform.New("")
	if err != nil {
		return err
	}
	return platform.SecurePermissions(path, isDir)
}
