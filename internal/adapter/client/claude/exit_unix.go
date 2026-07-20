//go:build !windows

package claude

import (
	"os"
	"syscall"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
)

func signalLaunchResult(state *os.ProcessState) (domainclient.LaunchResult, bool) {
	if state == nil {
		return domainclient.LaunchResult{}, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return domainclient.LaunchResult{}, false
	}
	signal := status.Signal()
	return domainclient.LaunchResult{ExitCode: 128 + int(signal), Signal: signal.String()}, true
}
