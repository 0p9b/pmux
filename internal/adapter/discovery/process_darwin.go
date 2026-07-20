//go:build darwin

package discovery

import (
	"context"
	"sort"

	"github.com/0p9b/pmux/internal/pmuxerr"
	"golang.org/x/sys/unix"
)

// LocalProcessEnumerator observes Darwin's process table and KERN_PROCARGS2.
// Per-process permission and exit races are expected and are skipped.
type LocalProcessEnumerator struct{}

func (LocalProcessEnumerator) Processes(ctx context.Context) ([]ProcessEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, processEnumerationCanceled(err)
	}
	argMax, err := unix.SysctlUint32("kern.argmax")
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not determine the Darwin process argument limit")
	}
	if argMax < 4 || argMax > maxProcessArgBytes {
		return nil, pmuxerr.Wrap(errProcessArgsTooLarge, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not safely bound Darwin process arguments")
	}

	entries, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read the Darwin process table")
	}
	if len(entries) > maxEnumeratedProcesses {
		return nil, pmuxerr.Wrap(errProcessArgsTooLarge, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Darwin process discovery exceeded its process limit")
	}

	processes := make([]ProcessEvidence, 0)
	seen := make(map[int]struct{})
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, processEnumerationCanceled(err)
		}
		pid := int(entries[i].Proc.P_pid)
		if pid <= 0 {
			continue
		}
		if _, duplicate := seen[pid]; duplicate {
			continue
		}
		seen[pid] = struct{}{}
		if !looksLikeCore(darwinProcessName(entries[i].Proc.P_comm[:]), nil) {
			continue
		}

		raw, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil || len(raw) > int(argMax) || len(raw) > maxProcessArgBytes {
			continue
		}
		executable, argv, err := parseDarwinProcArgs(raw)
		if err != nil || !looksLikeCore(executable, argv) {
			continue
		}
		processes = append(processes, normalizeProcessEvidence(ProcessEvidence{
			PID:        pid,
			Executable: executable,
			Argv:       argv,
		}))
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func darwinProcessName(value []byte) string {
	end := 0
	for end < len(value) && value[end] != 0 {
		end++
	}
	return string(value[:end])
}

func processEnumerationCanceled(err error) error {
	return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "process discovery was canceled")
}
