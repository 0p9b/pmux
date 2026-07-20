package windowstask

import (
	"context"
	"errors"
	"time"
)

// ErrTaskNotFound is returned by COM when a registered task does not exist.
var ErrTaskNotFound = errors.New("scheduled task not found")

// TaskState is the Task Scheduler 2.0 registered-task state. The COM
// implementation maps TASK_STATE values to this transport-neutral enum.
type TaskState string

const (
	TaskStateUnknown  TaskState = "unknown"
	TaskStateDisabled TaskState = "disabled"
	TaskStateQueued   TaskState = "queued"
	TaskStateReady    TaskState = "ready"
	TaskStateRunning  TaskState = "running"
)

// RunLevel is the Task Scheduler principal run level.
type RunLevel string

const (
	RunLevelLeastPrivilege RunLevel = "least-privilege"
)

// MultipleInstancesPolicy controls what happens when a task is already active.
type MultipleInstancesPolicy string

const (
	MultipleInstancesIgnoreNew MultipleInstancesPolicy = "ignore-new"
)

// ExecAction deliberately keeps the executable and arguments in separate
// fields. A COM implementation must populate IExecAction.Path and
// IExecAction.Arguments separately and must never construct schtasks /TR or a
// shell command.
type ExecAction struct {
	Executable      string
	Arguments       []string
	WorkingDirectory string
}

// TaskDefinition is the subset of a Task Scheduler 2.0 definition required by
// PMux. It represents one current-user, on-logon task.
type TaskDefinition struct {
	Name                   string
	Description            string
	Author                 string
	Enabled                bool
	LogonTrigger           bool
	RunOnlyWhenUserLoggedOn bool
	RunLevel               RunLevel
	MultipleInstances      MultipleInstancesPolicy
	RestartCount           int
	RestartInterval        time.Duration
	Exec                   ExecAction
}

// RegisteredTask is the observable state returned by Task Scheduler COM.
type RegisteredTask struct {
	Definition TaskDefinition
	State      TaskState
	PID        int
	Since      time.Time
	LastResult int32
}

// COM is the narrow Task Scheduler 2.0 COM boundary used by Manager. A Windows
// implementation owns COM initialization and marshaling; Manager owns PMux
// policy, identity, ownership, and lifecycle ordering.
type COM interface {
	GetTask(ctx context.Context, name string) (RegisteredTask, error)
	RegisterTaskDefinition(ctx context.Context, definition TaskDefinition) error
	DeleteTask(ctx context.Context, name string) error
	RunTask(ctx context.Context, name string) error
	StopTask(ctx context.Context, name string, timeout time.Duration) error
}
