//go:build !windows

package fs

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

func syncDirectoryPlatform(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
