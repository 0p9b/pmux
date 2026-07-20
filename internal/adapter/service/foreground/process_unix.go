//go:build !windows && !darwin

package foreground

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
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
	actualStarted, ok := linuxProcessStartedAt(pid)
	if !ok || actualStarted.Sub(started).Abs() > 3*time.Second {
		return false
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	expected, err := filepath.EvalSymlinks(spec.BinaryPath)
	if err != nil {
		expected = spec.BinaryPath
	}
	actual, err := filepath.EvalSymlinks(executable)
	if err != nil {
		actual = executable
	}
	if actual != expected {
		return false
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil || cwd != spec.RuntimeDir {
		return false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	parts := bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0})
	for i := 0; i+1 < len(parts); i++ {
		if string(parts[i]) == "-config" && string(parts[i+1]) == spec.ConfigPath {
			return true
		}
	}
	return false
}

func linuxProcessStartedAt(pid int) (time.Time, bool) {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, false
	}
	closing := bytes.LastIndexByte(body, ')')
	if closing < 0 {
		return time.Time{}, false
	}
	fields := bytes.Fields(body[closing+1:])
	// starttime is field 22 in proc_pid_stat(5); fields starts at field 3.
	if len(fields) <= 19 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	uptimeBody, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, false
	}
	uptimeFields := bytes.Fields(uptimeBody)
	if len(uptimeFields) == 0 {
		return time.Time{}, false
	}
	uptime, err := strconv.ParseFloat(string(uptimeFields[0]), 64)
	if err != nil {
		return time.Time{}, false
	}
	// Linux exports proc start ticks in USER_HZ, whose userspace ABI is 100 Hz.
	boot := time.Now().Add(-time.Duration(uptime * float64(time.Second)))
	return boot.Add(time.Duration(float64(ticks) / 100 * float64(time.Second))), true
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
