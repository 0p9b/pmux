package discovery

import (
	"encoding/binary"
	"errors"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxProcessArgBytes     = 4 << 20
	maxProcessArgCount     = 4096
	maxEnumeratedProcesses = 65536
)

var (
	errMalformedProcessArgs = errors.New("malformed process arguments")
	errProcessArgsTooLarge  = errors.New("process arguments exceed discovery limit")
)

// parseDarwinProcArgs decodes the KERN_PROCARGS2 layout: argc, executable,
// padding, then argc NUL-terminated argv values. Environment entries after
// argv are deliberately ignored.
func parseDarwinProcArgs(value []byte) (string, []string, error) {
	if len(value) < 4 {
		return "", nil, errMalformedProcessArgs
	}
	if len(value) > maxProcessArgBytes {
		return "", nil, errProcessArgsTooLarge
	}
	argc := int(int32(binary.LittleEndian.Uint32(value[:4])))
	if argc < 0 || argc > maxProcessArgCount {
		return "", nil, errProcessArgsTooLarge
	}

	cursor := 4
	executable, next, ok := nextNULTerminated(value, cursor)
	if !ok || executable == "" {
		return "", nil, errMalformedProcessArgs
	}
	cursor = next
	for cursor < len(value) && value[cursor] == 0 {
		cursor++
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc {
		arg, next, ok := nextNULTerminated(value, cursor)
		if !ok {
			return "", nil, errMalformedProcessArgs
		}
		argv = append(argv, arg)
		cursor = next
	}
	return cleanObservedPath(executable), argv, nil
}

func nextNULTerminated(value []byte, start int) (string, int, bool) {
	if start < 0 || start >= len(value) {
		return "", start, false
	}
	end := start
	for end < len(value) && value[end] != 0 {
		end++
	}
	if end == len(value) {
		return "", start, false
	}
	return strings.Clone(string(value[start:end])), end + 1, true
}

func cleanObservedPath(value string) string {
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return path.Clean(value)
	}
	return filepath.Clean(value)
}

func normalizeProcessEvidence(process ProcessEvidence) ProcessEvidence {
	process.Executable = cleanObservedPath(process.Executable)
	process.WorkingDir = cleanObservedPath(process.WorkingDir)
	process.Argv = append([]string(nil), process.Argv...)
	process.ConfigPath, _ = configFromArgv(process.Argv, process.WorkingDir)
	return process
}
