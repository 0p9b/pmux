//go:build !windows

package runtime

import "os"

func currentUID() int { return os.Getuid() }
