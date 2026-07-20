//go:build windows

package foreground

import (
	"errors"
	"os/exec"
)

// A cross-invocation foreground stop ends the Job Object process with a
// non-zero Windows status. The durable ownership check is what authorizes that
// stop, so the attached invocation treats this process exit as lifecycle
// completion rather than a core crash.
func stoppedByLifecycle(err error) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError)
}
