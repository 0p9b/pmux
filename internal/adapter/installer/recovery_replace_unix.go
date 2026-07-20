//go:build !windows

package installer

import "os"

func replaceRecoveryFile(source, destination string) error {
	return os.Rename(source, destination)
}
