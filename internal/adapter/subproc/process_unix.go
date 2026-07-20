//go:build !windows

package subproc

import (
	"os/exec"
	"syscall"
	"time"
)

func prepareProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		pid := command.Process.Pid
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return err
		}
		go func() {
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			<-timer.C
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}()
		return nil
	}
	command.WaitDelay = 6 * time.Second
}
