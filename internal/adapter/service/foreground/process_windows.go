//go:build windows

package foreground

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"golang.org/x/sys/windows"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func signalProcess(process *os.Process, signal os.Signal) error {
	return process.Signal(signal)
}

func killProcess(process *os.Process) error {
	return process.Kill()
}

func processOwned(pid int, spec service.ServiceSpec, started time.Time) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return false
	}
	actual := windows.UTF16ToString(buffer[:size])
	if !strings.EqualFold(filepath.Clean(actual), filepath.Clean(spec.BinaryPath)) {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return false
	}
	createdAt := time.Unix(0, created.Nanoseconds())
	delta := createdAt.Sub(started)
	return delta > -5*time.Second && delta < 5*time.Second
}

func stopExternal(ctx context.Context, pid int, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not open the recorded foreground CLIProxyAPI process")
	}
	defer windows.CloseHandle(handle)
	_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return pmuxerr.Wrap(ctx.Err(), pmuxerr.ServiceStartFailed, pmuxerr.Environment, "foreground CLIProxyAPI shutdown was interrupted")
		case <-deadline.C:
			if err := windows.TerminateProcess(handle, 1); err != nil {
				return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not force the recorded foreground CLIProxyAPI process to stop")
			}
			return nil
		case <-ticker.C:
			event, err := windows.WaitForSingleObject(handle, 0)
			if err == nil && event == windows.WAIT_OBJECT_0 {
				return nil
			}
		}
	}
}
