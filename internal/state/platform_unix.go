//go:build !windows

package state

import "os"

func replaceStateFile(source, destination string) error {
	return os.Rename(source, destination)
}

func syncStateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
