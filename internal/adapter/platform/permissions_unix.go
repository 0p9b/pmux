//go:build linux || darwin

package platform

import (
	"fmt"
	"os"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func secureUnixPermissions(path string, isDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return permissionError(err, path, "could not inspect private path")
	}
	if err := validateUnixPathType(info, path, isDir); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if isDir {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return permissionError(err, path, "could not secure private path")
	}
	return verifyUnixPermissions(path, isDir)
}

func verifyUnixPermissions(path string, isDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return permissionError(err, path, "could not inspect private path permissions")
	}
	if err := validateUnixPathType(info, path, isDir); err != nil {
		return err
	}
	expected := os.FileMode(0o600)
	if isDir {
		expected = 0o700
	}
	if actual := info.Mode().Perm(); actual != expected {
		return &pmuxerr.Error{
			Code:        pmuxerr.ConfigInsecurePermissions,
			Class:       pmuxerr.Environment,
			Message:     fmt.Sprintf("private path %q has permissions %04o; expected %04o", path, actual, expected),
			Explanation: "PMux private files and directories must not be accessible by group or other users",
			Evidence:    []string{fmt.Sprintf("path: %s", path), fmt.Sprintf("mode: %04o", actual)},
		}
	}
	return nil
}

func validateUnixPathType(info os.FileInfo, path string, isDir bool) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return pmuxerr.New(pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, fmt.Sprintf("refusing to change permissions through symbolic link %q", path))
	}
	if isDir && !info.IsDir() {
		return pmuxerr.New(pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, fmt.Sprintf("private directory path %q is not a directory", path))
	}
	if !isDir && !info.Mode().IsRegular() {
		return pmuxerr.New(pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, fmt.Sprintf("private file path %q is not a regular file", path))
	}
	return nil
}

func permissionError(err error, path, message string) *pmuxerr.Error {
	wrapped := pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, message)
	wrapped.Evidence = []string{"path: " + path}
	return wrapped
}
