//go:build linux

package discovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

// LocalProcessEnumerator observes Linux procfs without signaling or otherwise
// modifying any process.
type LocalProcessEnumerator struct {
	ProcRoot string
}

func (e LocalProcessEnumerator) Processes(ctx context.Context) ([]ProcessEvidence, error) {
	root := e.ProcRoot
	if root == "" {
		root = "/proc"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read Linux process metadata")
	}
	processes := make([]ProcessEvidence, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "process discovery was canceled")
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		base := filepath.Join(root, entry.Name())
		executable, exeErr := os.Readlink(filepath.Join(base, "exe"))
		if exeErr != nil {
			continue
		}
		cmdline, cmdErr := os.ReadFile(filepath.Join(base, "cmdline"))
		if cmdErr != nil {
			continue
		}
		argv := splitNUL(cmdline)
		if !looksLikeCore(executable, argv) {
			continue
		}
		workingDir, _ := os.Readlink(filepath.Join(base, "cwd"))
		configPath, _ := configFromArgv(argv, workingDir)
		processes = append(processes, ProcessEvidence{PID: pid, Executable: executable, Argv: argv, WorkingDir: workingDir, ConfigPath: configPath})
	}
	return processes, nil
}

func splitNUL(value []byte) []string {
	parts := bytes.Split(bytes.TrimRight(value, "\x00"), []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			out = append(out, strings.Clone(string(part)))
		}
	}
	return out
}
