//go:build !windows

package foreground

import (
	"errors"
	"os/exec"
	"syscall"
)

func stoppedByLifecycle(err error) bool {
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return false
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	switch status.Signal() {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
		return true
	default:
		return false
	}
}
