//go:build windows

package windowstask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

const (
	taskCreateOrUpdate        = 6
	taskLogonInteractiveToken = 3
	taskTriggerLogon          = 9
	taskActionExec            = 0
)

// NewNativeCOM returns the current-user Task Scheduler 2.0 COM transport. Each
// operation owns one COM apartment on a locked OS thread; no shell command or
// localized command output is involved.
func NewNativeCOM() COM { return nativeCOM{} }

// NewNativeLogReader opens the private service-host log written by PMux.
func NewNativeLogReader() LogReader { return nativeLogReader{} }

type nativeCOM struct{}

type nativeLogReader struct{}

func (nativeCOM) GetTask(ctx context.Context, name string) (RegisteredTask, error) {
	var out RegisteredTask
	err := withTaskFolder(ctx, func(_ *ole.IDispatch, folder *ole.IDispatch) error {
		value, err := oleutil.CallMethod(folder, "GetTask", name)
		if err != nil {
			if taskMissing(err) {
				return ErrTaskNotFound
			}
			return err
		}
		task := value.ToIDispatch()
		if task == nil {
			return ErrTaskNotFound
		}
		defer task.Release()
		definition, err := readDefinition(task, name)
		if err != nil {
			return err
		}
		out.Definition = definition
		state, err := intProperty(task, "State")
		if err != nil {
			return err
		}
		out.State = taskState(state)
		last, err := intProperty(task, "LastTaskResult")
		if err == nil {
			out.LastResult = int32(last)
		}
		instancesValue, instancesErr := oleutil.CallMethod(task, "GetInstances", 0)
		if instancesErr == nil {
			instances := instancesValue.ToIDispatch()
			if instances != nil {
				defer instances.Release()
				count, _ := intProperty(instances, "Count")
				if count > 0 {
					itemValue, itemErr := oleutil.CallMethod(instances, "Item", 1)
					if itemErr == nil {
						item := itemValue.ToIDispatch()
						if item != nil {
							out.PID, _ = intProperty(item, "EnginePID")
							item.Release()
						}
					}
				}
			}
		}
		return nil
	})
	return out, err
}

func (nativeCOM) RegisterTaskDefinition(ctx context.Context, definition TaskDefinition) error {
	return withTaskFolder(ctx, func(service, folder *ole.IDispatch) error {
		value, err := oleutil.CallMethod(service, "NewTask", 0)
		if err != nil {
			return err
		}
		task := value.ToIDispatch()
		if task == nil {
			return errors.New("Task Scheduler returned no task definition")
		}
		defer task.Release()

		registration, err := getDispatch(task, "RegistrationInfo")
		if err != nil {
			return err
		}
		if _, err = oleutil.PutProperty(registration, "Description", definition.Description); err == nil {
			_, err = oleutil.PutProperty(registration, "Author", definition.Author)
		}
		registration.Release()
		if err != nil {
			return err
		}

		principal, err := getDispatch(task, "Principal")
		if err != nil {
			return err
		}
		_, err = oleutil.PutProperty(principal, "LogonType", taskLogonInteractiveToken)
		if err == nil {
			_, err = oleutil.PutProperty(principal, "RunLevel", 0)
		}
		principal.Release()
		if err != nil {
			return err
		}

		settings, err := getDispatch(task, "Settings")
		if err != nil {
			return err
		}
		for _, property := range []struct {
			name  string
			value any
		}{
			{"Enabled", definition.Enabled}, {"StartWhenAvailable", true},
			{"MultipleInstances", 2}, {"RestartCount", definition.RestartCount},
			{"RestartInterval", isoDuration(definition.RestartInterval)},
		} {
			if _, err = oleutil.PutProperty(settings, property.name, property.value); err != nil {
				break
			}
		}
		settings.Release()
		if err != nil {
			return err
		}

		if definition.LogonTrigger {
			triggers, getErr := getDispatch(task, "Triggers")
			if getErr != nil {
				return getErr
			}
			created, createErr := oleutil.CallMethod(triggers, "Create", taskTriggerLogon)
			if created != nil && created.ToIDispatch() != nil {
				created.ToIDispatch().Release()
			}
			triggers.Release()
			if createErr != nil {
				return createErr
			}
		}

		actions, err := getDispatch(task, "Actions")
		if err != nil {
			return err
		}
		created, err := oleutil.CallMethod(actions, "Create", taskActionExec)
		actions.Release()
		if err != nil {
			return err
		}
		action := created.ToIDispatch()
		if action == nil {
			return errors.New("Task Scheduler returned no ExecAction")
		}
		defer action.Release()
		if _, err = oleutil.PutProperty(action, "Path", definition.Exec.Executable); err != nil {
			return err
		}
		if _, err = oleutil.PutProperty(action, "Arguments", windows.ComposeCommandLine(definition.Exec.Arguments)); err != nil {
			return err
		}
		if _, err = oleutil.PutProperty(action, "WorkingDirectory", definition.Exec.WorkingDirectory); err != nil {
			return err
		}

		_, err = oleutil.CallMethod(folder, "RegisterTaskDefinition", definition.Name, task, taskCreateOrUpdate, nil, nil, taskLogonInteractiveToken, nil)
		return err
	})
}

func (nativeCOM) DeleteTask(ctx context.Context, name string) error {
	return withTaskFolder(ctx, func(_ *ole.IDispatch, folder *ole.IDispatch) error {
		_, err := oleutil.CallMethod(folder, "DeleteTask", name, 0)
		if taskMissing(err) {
			return ErrTaskNotFound
		}
		return err
	})
}

func (nativeCOM) RunTask(ctx context.Context, name string) error {
	return withRegisteredTask(ctx, name, func(task *ole.IDispatch) error {
		value, err := oleutil.CallMethod(task, "Run", nil)
		if value != nil && value.ToIDispatch() != nil {
			value.ToIDispatch().Release()
		}
		return err
	})
}

func (nativeCOM) StopTask(ctx context.Context, name string, timeout time.Duration) error {
	if err := withRegisteredTask(ctx, name, func(task *ole.IDispatch) error {
		_, err := oleutil.CallMethod(task, "Stop", 0)
		return err
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		task, err := (nativeCOM{}).GetTask(ctx, name)
		if err != nil {
			return err
		}
		if task.State != TaskStateRunning && task.State != TaskStateQueued {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("Task Scheduler did not stop %q within %s", name, timeout)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func withRegisteredTask(ctx context.Context, name string, fn func(*ole.IDispatch) error) error {
	return withTaskFolder(ctx, func(_ *ole.IDispatch, folder *ole.IDispatch) error {
		value, err := oleutil.CallMethod(folder, "GetTask", name)
		if err != nil {
			if taskMissing(err) {
				return ErrTaskNotFound
			}
			return err
		}
		task := value.ToIDispatch()
		if task == nil {
			return ErrTaskNotFound
		}
		defer task.Release()
		return fn(task)
	})
}

func withTaskFolder(ctx context.Context, fn func(*ole.IDispatch, *ole.IDispatch) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitialize(0); err != nil {
		return err
	}
	defer ole.CoUninitialize()
	unknown, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		return err
	}
	defer unknown.Release()
	service, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer service.Release()
	if _, err := oleutil.CallMethod(service, "Connect"); err != nil {
		return err
	}
	value, err := oleutil.CallMethod(service, "GetFolder", `\`)
	if err != nil {
		return err
	}
	folder := value.ToIDispatch()
	if folder == nil {
		return errors.New("Task Scheduler root folder is unavailable")
	}
	defer folder.Release()
	return fn(service, folder)
}

func readDefinition(task *ole.IDispatch, name string) (TaskDefinition, error) {
	out := TaskDefinition{Name: name, RunOnlyWhenUserLoggedOn: true, RunLevel: RunLevelLeastPrivilege, MultipleInstances: MultipleInstancesIgnoreNew}
	definition, err := getDispatch(task, "Definition")
	if err != nil {
		return out, err
	}
	defer definition.Release()
	registration, err := getDispatch(definition, "RegistrationInfo")
	if err != nil {
		return out, err
	}
	out.Description, _ = stringProperty(registration, "Description")
	out.Author, _ = stringProperty(registration, "Author")
	registration.Release()
	settings, err := getDispatch(definition, "Settings")
	if err != nil {
		return out, err
	}
	out.Enabled, _ = boolProperty(settings, "Enabled")
	out.RestartCount, _ = intProperty(settings, "RestartCount")
	interval, _ := stringProperty(settings, "RestartInterval")
	out.RestartInterval = parseISODuration(interval)
	settings.Release()
	triggers, err := getDispatch(definition, "Triggers")
	if err != nil {
		return out, err
	}
	count, _ := intProperty(triggers, "Count")
	out.LogonTrigger = count > 0
	triggers.Release()
	actions, err := getDispatch(definition, "Actions")
	if err != nil {
		return out, err
	}
	count, _ = intProperty(actions, "Count")
	if count < 1 {
		actions.Release()
		return out, errors.New("Task Scheduler definition has no action")
	}
	value, err := oleutil.CallMethod(actions, "Item", 1)
	actions.Release()
	if err != nil {
		return out, err
	}
	action := value.ToIDispatch()
	if action == nil {
		return out, errors.New("Task Scheduler action is unavailable")
	}
	defer action.Release()
	out.Exec.Executable, _ = stringProperty(action, "Path")
	arguments, _ := stringProperty(action, "Arguments")
	out.Exec.Arguments, err = windows.DecomposeCommandLine(arguments)
	if err != nil {
		return out, err
	}
	out.Exec.WorkingDirectory, _ = stringProperty(action, "WorkingDirectory")
	return out, nil
}

func getDispatch(object *ole.IDispatch, name string) (*ole.IDispatch, error) {
	value, err := oleutil.GetProperty(object, name)
	if err != nil {
		return nil, err
	}
	dispatch := value.ToIDispatch()
	if dispatch == nil {
		return nil, fmt.Errorf("Task Scheduler property %s is unavailable", name)
	}
	return dispatch, nil
}
func stringProperty(object *ole.IDispatch, name string) (string, error) {
	value, err := oleutil.GetProperty(object, name)
	if err != nil {
		return "", err
	}
	return value.ToString(), nil
}
func intProperty(object *ole.IDispatch, name string) (int, error) {
	value, err := oleutil.GetProperty(object, name)
	if err != nil {
		return 0, err
	}
	switch v := value.Value().(type) {
	case int:
		return v, nil
	case int8:
		return int(v), nil
	case int16:
		return int(v), nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case uint:
		return int(v), nil
	case uint32:
		return int(v), nil
	default:
		return int(value.Val), nil
	}
}
func boolProperty(object *ole.IDispatch, name string) (bool, error) {
	value, err := oleutil.GetProperty(object, name)
	if err != nil {
		return false, err
	}
	if v, ok := value.Value().(bool); ok {
		return v, nil
	}
	return value.Val != 0, nil
}
func taskState(value int) TaskState {
	switch value {
	case 1:
		return TaskStateDisabled
	case 2:
		return TaskStateQueued
	case 3:
		return TaskStateReady
	case 4:
		return TaskStateRunning
	default:
		return TaskStateUnknown
	}
}
func taskMissing(err error) bool {
	var oleErr *ole.OleError
	return errors.As(err, &oleErr) && (uint32(oleErr.Code()) == 0x80070002 || uint32(oleErr.Code()) == 0x8004130f)
}
func isoDuration(value time.Duration) string {
	if value <= 0 {
		return "PT0S"
	}
	seconds := int64(value / time.Second)
	if seconds%60 == 0 {
		return fmt.Sprintf("PT%dM", seconds/60)
	}
	return fmt.Sprintf("PT%dS", seconds)
}
func parseISODuration(value string) time.Duration {
	var number int64
	if _, err := fmt.Sscanf(strings.ToUpper(value), "PT%dM", &number); err == nil {
		return time.Duration(number) * time.Minute
	}
	if _, err := fmt.Sscanf(strings.ToUpper(value), "PT%dS", &number); err == nil {
		return time.Duration(number) * time.Second
	}
	return 0
}

func (nativeLogReader) Open(ctx context.Context, _ string, logDir string, tail int, follow bool) (io.ReadCloser, error) {
	path := filepath.Join(logDir, "proxy.log")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if tail > 0 {
		lines := bytes.Split(body, []byte{'\n'})
		if len(lines) > tail+1 {
			lines = lines[len(lines)-tail-1:]
		}
		body = bytes.Join(lines, []byte{'\n'})
	}
	if !follow {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(int64(len(body)), io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	go func() { <-ctx.Done(); _ = file.Close() }()
	return file, nil
}
