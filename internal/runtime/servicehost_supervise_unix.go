//go:build !windows

package runtime

import "os/exec"

// superviseChild is a no-op on Unix: launchd and systemd track the whole
// process group/session and reap children when the service stops.
func superviseChild(_ *exec.Cmd) error { return nil }
