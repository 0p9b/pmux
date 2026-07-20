package discovery

import (
	"context"
	"errors"
	"strings"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	defaultScheduledTaskLimit = 4096
	maxScheduledActions       = 16
	windowsOwnershipPrefix    = "PMux-managed CLIProxyAPI task; instance="
)

type TaskState string

const (
	TaskStateUnknown  TaskState = "unknown"
	TaskStateDisabled TaskState = "disabled"
	TaskStateQueued   TaskState = "queued"
	TaskStateReady    TaskState = "ready"
	TaskStateRunning  TaskState = "running"
)

// ScheduledExecAction is the read-only ExecAction projection returned by the
// Task Scheduler 2.0 COM source. Executable and Arguments remain separate;
// Arguments is parsed as a Windows command line without invoking a shell.
type ScheduledExecAction struct {
	Executable      string
	Arguments       string
	WorkingDirectory string
}

// ScheduledTask is the bounded, non-secret Task Scheduler evidence consumed by
// WindowsServiceEnumerator.
type ScheduledTask struct {
	Identity    string
	Definition  string
	Description string
	State       TaskState
	Actions     []ScheduledExecAction
}

// ScheduledTaskSource is the read-only Task Scheduler 2.0 COM boundary.
type ScheduledTaskSource interface {
	Tasks(ctx context.Context, limit int) ([]ScheduledTask, error)
}

// WindowsServiceEnumerator discovers existing CLIProxyAPI scheduled tasks. It
// never registers, starts, stops, deletes, or updates a task.
type WindowsServiceEnumerator struct {
	Source ScheduledTaskSource
	Limit  int
}

func (e WindowsServiceEnumerator) Services(ctx context.Context) ([]ServiceEvidence, error) {
	if e.Source == nil {
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Windows Task Scheduler discovery is unavailable.")
	}
	limit := e.Limit
	if limit <= 0 || limit > defaultScheduledTaskLimit {
		limit = defaultScheduledTaskLimit
	}
	tasks, err := e.Source.Tasks(ctx, limit)
	if err != nil {
		var pe *pmuxerr.Error
		if errors.As(err, &pe) {
			return nil, pe
		}
		return nil, &pmuxerr.Error{
			Code:        pmuxerr.ServiceBackendUnavailable,
			Class:       pmuxerr.Environment,
			Message:     "Could not enumerate Windows Scheduled Tasks through Task Scheduler 2.0 COM.",
			Explanation: "Read-only service discovery could not connect to the current user's Task Scheduler view.",
			Cause:       err,
		}
	}
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}

	results := make([]ServiceEvidence, 0, len(tasks))
	for _, task := range tasks {
		owned := windowsTaskOwned(task.Identity, task.Description)
		actions := task.Actions
		if len(actions) > maxScheduledActions {
			actions = actions[:maxScheduledActions]
		}
		for _, action := range actions {
			argv := append([]string{action.Executable}, parseWindowsCommandLine(action.Arguments)...)
			if !owned && !looksLikeCore(action.Executable, argv) && !strings.Contains(strings.ToLower(task.Identity), "cliproxy") {
				continue
			}
			evidence := ServiceEvidence{
				Backend:    service.BackendWindowsTask,
				Identity:   task.Identity,
				Definition: task.Definition,
				Executable: action.Executable,
				Argv:       argv,
				WorkingDir: action.WorkingDirectory,
				PMuxOwned:  owned,
				State:      mapScheduledTaskState(task.State),
			}
			evidence.ConfigPath, _ = configFromWindowsArgv(evidence.Argv, evidence.WorkingDir)
			results = append(results, evidence)
			break
		}
	}
	return results, nil
}

func windowsTaskOwned(identity, description string) bool {
	instanceID, ok := strings.CutPrefix(description, windowsOwnershipPrefix)
	return ok && instanceID != "" && identity == service.Identity(service.BackendWindowsTask, instanceID)
}

func mapScheduledTaskState(state TaskState) service.ServiceState {
	switch state {
	case TaskStateDisabled, TaskStateReady:
		return service.ServiceStopped
	case TaskStateQueued:
		return service.ServiceStarting

	case TaskStateRunning:
		return service.ServiceRunning
	default:
		return service.ServiceUnknown
	}
}
func configFromWindowsArgv(argv []string, workingDirectory string) (string, bool) {
	for index, argument := range argv {
		var value string
		switch {
		case (argument == "-config" || argument == "--config") && index+1 < len(argv):
			value = argv[index+1]
		case strings.HasPrefix(argument, "-config="):
			value = strings.TrimPrefix(argument, "-config=")
		case strings.HasPrefix(argument, "--config="):
			value = strings.TrimPrefix(argument, "--config=")
		default:
			continue
		}
		if isWindowsAbsolutePath(value) {
			return value, true
		}
		return resolveProcessPath(value, workingDirectory), true
	}
	if workingDirectory == "" {
		return "", false
	}
	if strings.HasSuffix(workingDirectory, `\`) || strings.HasSuffix(workingDirectory, "/") {
		return workingDirectory + "config.yaml", false
	}
	return workingDirectory + `\config.yaml`, false
}

func isWindowsAbsolutePath(path string) bool {
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	return len(path) >= 3 &&
		((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}


// parseWindowsCommandLine implements the CommandLineToArgvW backslash/quote
// rules for an ExecAction Arguments field. It does not expand variables,
// evaluate metacharacters, or invoke cmd.exe/PowerShell.
func parseWindowsCommandLine(input string) []string {
	var result []string
	for index := 0; ; {
		for index < len(input) && (input[index] == ' ' || input[index] == '\t') {
			index++
		}
		if index >= len(input) {
			return result
		}

		var argument strings.Builder
		inQuotes := false
		started := false
		for index < len(input) {
			if !inQuotes && (input[index] == ' ' || input[index] == '\t') {
				break
			}
			started = true
			backslashes := 0
			for index < len(input) && input[index] == '\\' {
				backslashes++
				index++
			}
			if index < len(input) && input[index] == '"' {
				for range backslashes / 2 {
					argument.WriteByte('\\')
				}
				if backslashes%2 == 1 {
					argument.WriteByte('"')
					index++
					continue
				}
				if inQuotes && index+1 < len(input) && input[index+1] == '"' {
					argument.WriteByte('"')
					index += 2
					continue
				}
				inQuotes = !inQuotes
				index++
				continue
			}
			for range backslashes {
				argument.WriteByte('\\')
			}
			if index < len(input) {
				argument.WriteByte(input[index])
				index++
			}
		}
		if started {
			result = append(result, argument.String())
		}
	}
}
