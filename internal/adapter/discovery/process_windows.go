//go:build windows

package discovery

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"unsafe"

	"github.com/0p9b/pmux/internal/pmuxerr"
	"golang.org/x/sys/windows"
)

const processCommandLineInformation = 60

// LocalProcessEnumerator observes the Windows process snapshot and queries
// image paths and command lines through native APIs. Processes that exit or
// deny query access are skipped.
type LocalProcessEnumerator struct{}

func (LocalProcessEnumerator) Processes(ctx context.Context) ([]ProcessEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, windowsProcessEnumerationCanceled(err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not create the Windows process snapshot")
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read the Windows process snapshot")
	}

	processes := make([]ProcessEvidence, 0)
	seen := make(map[uint32]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return nil, windowsProcessEnumerationCanceled(err)
		}
		if len(seen) >= maxEnumeratedProcesses {
			return nil, pmuxerr.Wrap(errProcessArgsTooLarge, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Windows process discovery exceeded its process limit")
		}
		pid := entry.ProcessID
		if pid != 0 {
			if _, duplicate := seen[pid]; !duplicate {
				seen[pid] = struct{}{}
				name := windows.UTF16ToString(entry.ExeFile[:])
				if looksLikeCore(name, nil) {
					if process, ok := observeWindowsProcess(pid); ok {
						processes = append(processes, process)
					}
				}
			}
		}

		entry.Size = uint32(unsafe.Sizeof(entry))
		err := windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not continue reading the Windows process snapshot")
		}
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func observeWindowsProcess(pid uint32) (ProcessEvidence, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ProcessEvidence{}, false
	}
	defer windows.CloseHandle(handle)

	executable, err := windowsProcessImage(handle)
	if err != nil {
		return ProcessEvidence{}, false
	}
	argv, err := windowsProcessArgv(handle)
	if err != nil || len(argv) == 0 || !looksLikeCore(executable, argv) {
		return ProcessEvidence{}, false
	}
	return normalizeProcessEvidence(ProcessEvidence{
		PID:        int(pid),
		Executable: executable,
		Argv:       argv,
	}), true
}

func windowsProcessImage(handle windows.Handle) (string, error) {
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	if size == 0 || size > uint32(len(buffer)) {
		return "", errMalformedProcessArgs
	}
	return cleanObservedPath(windows.UTF16ToString(buffer[:size])), nil
}

type windowsUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func windowsProcessArgv(handle windows.Handle) ([]string, error) {
	var required uint32
	_ = windows.NtQueryInformationProcess(handle, processCommandLineInformation, nil, 0, &required)
	if required < uint32(unsafe.Sizeof(windowsUnicodeString{})) || required > maxProcessArgBytes {
		return nil, errProcessArgsTooLarge
	}

	buffer := make([]byte, required)
	for range 2 {
		returned := uint32(0)
		err := windows.NtQueryInformationProcess(handle, processCommandLineInformation, unsafe.Pointer(&buffer[0]), uint32(len(buffer)), &returned)
		if err == nil {
			argv, parseErr := argvFromWindowsProcessBuffer(buffer)
			runtime.KeepAlive(buffer)
			return argv, parseErr
		}
		if returned <= uint32(len(buffer)) || returned > maxProcessArgBytes {
			return nil, err
		}
		buffer = make([]byte, returned)
	}
	return nil, errMalformedProcessArgs
}

func argvFromWindowsProcessBuffer(buffer []byte) ([]string, error) {
	if len(buffer) < int(unsafe.Sizeof(windowsUnicodeString{})) {
		return nil, errMalformedProcessArgs
	}
	header := (*windowsUnicodeString)(unsafe.Pointer(&buffer[0]))
	length := uintptr(header.Length)
	if length == 0 || length%2 != 0 || length > maxProcessArgBytes {
		return nil, errMalformedProcessArgs
	}
	start := uintptr(unsafe.Pointer(&buffer[0]))
	end := start + uintptr(len(buffer))
	text := uintptr(unsafe.Pointer(header.Buffer))
	if text < start || text > end || length > end-text {
		return nil, errMalformedProcessArgs
	}
	commandLine := windows.UTF16ToString(unsafe.Slice(header.Buffer, length/2))
	if commandLine == "" {
		return nil, errMalformedProcessArgs
	}
	argv, err := windows.DecomposeCommandLine(commandLine)
	if err != nil || len(argv) > maxProcessArgCount {
		if err != nil {
			return nil, err
		}
		return nil, errProcessArgsTooLarge
	}
	return argv, nil
}

func windowsProcessEnumerationCanceled(err error) error {
	return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "process discovery was canceled")
}
