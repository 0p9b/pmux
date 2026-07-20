//go:build windows

package subproc

import (
	"os/exec"
	"syscall"
	"time"
)

// CREATE_NEW_PROCESS_GROUP permits graceful console interruption. The native
// Windows service adapter owns Job Object supervision for persistent children.
func prepareProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	command.WaitDelay = 5 * time.Second
}
