//go:build windows

package claude

import (
	"os"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
)

func signalLaunchResult(*os.ProcessState) (domainclient.LaunchResult, bool) {
	return domainclient.LaunchResult{}, false
}
