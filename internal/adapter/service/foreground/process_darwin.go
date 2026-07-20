//go:build darwin

package foreground

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"golang.org/x/sys/unix"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcess(process *os.Process, signal os.Signal) error {
	if unixSignal, ok := signal.(syscall.Signal); ok {
		return syscall.Kill(-process.Pid, unixSignal)
	}
	return process.Signal(signal)
}

func killProcess(process *os.Process) error {
	return syscall.Kill(-process.Pid, syscall.SIGKILL)
}

func processOwned(pid int, spec service.ServiceSpec, started time.Time) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(info.Proc.P_pid) != pid {
		return false
	}
	processStarted := time.Unix(info.Proc.P_starttime.Sec, int64(info.Proc.P_starttime.Usec)*1000)
	if processStarted.Sub(started).Abs() > 3*time.Second {
		return false
	}
	nameBytes := info.Proc.P_comm[:]
	if end := bytes.IndexByte(nameBytes, 0); end >= 0 {
		nameBytes = nameBytes[:end]
	}
	return string(nameBytes) == filepath.Base(spec.BinaryPath)
}

func stopExternal(ctx context.Context, pid int, timeout time.Duration) error {
	if err := syscall.Kill(-pid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not signal the recorded foreground CLIProxyAPI process group")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return pmuxerr.Wrap(ctx.Err(), pmuxerr.ServiceStartFailed, pmuxerr.Environment, "foreground CLIProxyAPI shutdown was interrupted")
		case <-deadline.C:
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not force the recorded foreground CLIProxyAPI process group to stop")
			}
			return nil
		case <-ticker.C:
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				return nil
			}
		}
	}
}
