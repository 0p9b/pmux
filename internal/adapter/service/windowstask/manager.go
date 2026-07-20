package windowstask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	ownershipPrefix    = "PMux-managed CLIProxyAPI task; instance="
	taskAuthor         = "PMux"
	defaultStopTimeout = 15 * time.Second
)

// PermissionVerifier is satisfied by the Windows platform adapter. Scheduled
// task registration and activation fail closed when any private path no longer
// has the required current-user-and-SYSTEM-only DACL.
type PermissionVerifier interface {
	VerifySecurePermissions(path string, isDir bool) error
}

// LogReader opens PMux-managed service-host logs. Task Scheduler history and
// localized command output are intentionally not parsed.
type LogReader interface {
	Open(ctx context.Context, instanceID, logDir string, tail int, follow bool) (io.ReadCloser, error)
}

// Manager implements the exact domain ServiceManager contract for the
// explicitly selected Windows Scheduled Task backend. Foreground selection is
// handled by the foreground adapter and remains the Windows default.
type Manager struct {
	com         COM
	permissions PermissionVerifier
	logs        LogReader
	health      health.Checker

	mu          sync.RWMutex
	spec        service.ServiceSpec
	lastHealth  health.Result
	healthKnown bool
}

var _ service.ServiceManager = (*Manager)(nil)

// New binds a manager to one installation. The ServiceSpec must use the
// canonical Task Scheduler identity for its instance.
func New(spec service.ServiceSpec, com COM, permissions PermissionVerifier, logs LogReader, healthChecker health.Checker) (*Manager, error) {
	if com == nil || permissions == nil || logs == nil || healthChecker == nil {
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Windows Task Scheduler dependencies are unavailable.")
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	return &Manager{com: com, permissions: permissions, logs: logs, health: healthChecker, spec: cloneSpec(spec)}, nil
}

func (m *Manager) Backend() service.ServiceBackend { return service.BackendWindowsTask }

func (m *Manager) Detect(ctx context.Context) (service.ServiceStatus, error) {
	return m.readStatus(ctx)
}

func (m *Manager) Install(ctx context.Context, spec service.ServiceSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	bound := m.currentSpec()
	if spec.InstanceID != bound.InstanceID {
		return ownershipError(service.Identity(service.BackendWindowsTask, spec.InstanceID), "the adapter is bound to a different PMux instance")
	}
	if err := m.verifyPrivatePaths(spec, "installed"); err != nil {
		return err
	}

	name := service.Identity(service.BackendWindowsTask, spec.InstanceID)
	existing, err := m.com.GetTask(ctx, name)
	if err != nil && !errors.Is(err, ErrTaskNotFound) {
		return backendError(err, "Could not inspect the Windows Scheduled Task.")
	}
	if err == nil && !isOwned(existing, spec.InstanceID) {
		return ownershipError(name, "a foreign task already uses the canonical identity")
	}

	definition := definitionFor(spec)
	if err := m.com.RegisterTaskDefinition(ctx, definition); err != nil {
		return backendError(err, "Could not register the Windows Scheduled Task through Task Scheduler 2.0 COM.")
	}

	m.mu.Lock()
	m.spec = cloneSpec(spec)
	m.lastHealth = health.Result{}
	m.healthKnown = false
	m.mu.Unlock()
	return nil
}

func (m *Manager) Uninstall(ctx context.Context) error {
	spec := m.currentSpec()
	name := service.Identity(service.BackendWindowsTask, spec.InstanceID)
	task, err := m.com.GetTask(ctx, name)
	if errors.Is(err, ErrTaskNotFound) {
		return nil
	}
	if err != nil {
		return backendError(err, "Could not inspect the Windows Scheduled Task before uninstalling it.")
	}
	if !isOwned(task, spec.InstanceID) {
		return ownershipError(name, "PMux did not create this task")
	}
	if task.State == TaskStateRunning || task.State == TaskStateQueued {
		if err := m.com.StopTask(ctx, name, defaultStopTimeout); err != nil {
			return backendError(err, "Could not stop the Windows Scheduled Task before uninstalling it.")
		}
	}
	if err := m.com.DeleteTask(ctx, name); err != nil {
		return backendError(err, "Could not delete the PMux-owned Windows Scheduled Task.")
	}
	m.clearHealth()
	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	spec := m.currentSpec()
	name := service.Identity(service.BackendWindowsTask, spec.InstanceID)
	task, err := m.com.GetTask(ctx, name)
	if errors.Is(err, ErrTaskNotFound) {
		return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "The PMux Windows Scheduled Task is not installed.")
	}
	if err != nil {
		return backendError(err, "Could not inspect the Windows Scheduled Task before starting it.")
	}
	if !isOwned(task, spec.InstanceID) {
		return ownershipError(name, "PMux did not create this task")
	}
	if !reflect.DeepEqual(task.Definition, definitionFor(spec)) {
		return ownershipError(name, "the PMux task definition changed outside PMux")
	}
	if err := m.verifyPrivatePaths(spec, "started"); err != nil {
		return err
	}
	if task.State != TaskStateRunning {
		if err := m.com.RunTask(ctx, name); err != nil {
			return &pmuxerr.Error{
				Code:        pmuxerr.ServiceStartFailed,
				Class:       pmuxerr.Environment,
				Message:     "CLIProxyAPI could not be started by Windows Task Scheduler.",
				Explanation: "The PMux-owned task was registered, but Task Scheduler 2.0 COM rejected its start request.",
				Evidence:    []string{"task: " + name},
				Repair:      []string{"Run `pmux doctor` to inspect the scheduled task and PMux-managed logs."},
				Cause:       err,
			}
		}
	}
	result, err := m.health.WaitReady(ctx)
	if err != nil {
		var pe *pmuxerr.Error
		if errors.As(err, &pe) {
			return pe
		}
		return &pmuxerr.Error{
			Code:        pmuxerr.ServiceHealthDeadline,
			Class:       pmuxerr.Environment,
			Message:     "CLIProxyAPI started but did not become healthy within 15 seconds.",
			Explanation: "The Windows Scheduled Task is active, but the canonical /healthz gate did not pass.",
			Evidence:    []string{"task: " + name},
			Repair:      []string{"Run `pmux doctor` and inspect `pmux service logs`."},
			Cause:       err,
		}
	}
	m.mu.Lock()
	m.lastHealth = result
	m.healthKnown = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) Stop(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Service stop timeout must be greater than zero.")
	}
	spec := m.currentSpec()
	name := service.Identity(service.BackendWindowsTask, spec.InstanceID)
	task, err := m.com.GetTask(ctx, name)
	if errors.Is(err, ErrTaskNotFound) {
		return nil
	}
	if err != nil {
		return backendError(err, "Could not inspect the Windows Scheduled Task before stopping it.")
	}
	if !isOwned(task, spec.InstanceID) {
		return ownershipError(name, "PMux did not create this task")
	}
	if task.State == TaskStateRunning || task.State == TaskStateQueued {
		if err := m.com.StopTask(ctx, name, timeout); err != nil {
			return backendError(err, "Could not stop the PMux-owned Windows Scheduled Task.")
		}
	}
	m.clearHealth()
	return nil
}

func (m *Manager) Restart(ctx context.Context) (service.ServiceStatus, error) {
	if err := m.Stop(ctx, defaultStopTimeout); err != nil {
		return service.ServiceStatus{}, err
	}
	if err := m.Start(ctx); err != nil {
		return service.ServiceStatus{}, err
	}
	return m.readStatus(ctx)
}

func (m *Manager) Status(ctx context.Context) (service.ServiceStatus, error) {
	return m.readStatus(ctx)
}

func (m *Manager) Logs(ctx context.Context, tail int, follow bool) (io.ReadCloser, error) {
	if tail < 0 {
		return nil, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Log tail count must not be negative.")
	}
	spec := m.currentSpec()
	if err := m.verifyOne(spec.LogDir, true, "read logs"); err != nil {
		return nil, err
	}
	reader, err := m.logs.Open(ctx, spec.InstanceID, spec.LogDir, tail, follow)
	if err != nil {
		return nil, backendError(err, "Could not open the PMux-managed Windows service logs.")
	}
	return reader, nil
}

func (m *Manager) readStatus(ctx context.Context) (service.ServiceStatus, error) {
	spec := m.currentSpec()
	name := service.Identity(service.BackendWindowsTask, spec.InstanceID)
	task, err := m.com.GetTask(ctx, name)
	if errors.Is(err, ErrTaskNotFound) {
		return service.ServiceStatus{Backend: service.BackendWindowsTask, State: service.ServiceNotInstalled, CoreVersion: health.UnknownVersion}, nil
	}
	if err != nil {
		return service.ServiceStatus{}, backendError(err, "Could not query Windows Task Scheduler through COM.")
	}
	if !isOwned(task, spec.InstanceID) {
		return service.ServiceStatus{
			Backend:     service.BackendWindowsTask,
			State:       service.ServiceUnknown,
			Detail:      "foreign task occupies canonical identity " + name,
			CoreVersion: health.UnknownVersion,
			Warning:     "PMux will not modify this task.",
		}, nil
	}

	status := service.ServiceStatus{
		Backend:     service.BackendWindowsTask,
		State:       mapTaskState(task),
		PID:         task.PID,
		Since:       task.Since,
		Detail:      statusDetail(task),
		CoreVersion: health.UnknownVersion,
	}
	m.mu.RLock()
	if status.State == service.ServiceRunning && m.healthKnown {
		status.Healthy = true
		status.CoreVersion = m.lastHealth.Version
		if status.CoreVersion == "" {
			status.CoreVersion = health.UnknownVersion
		}
		status.Warning = m.lastHealth.Warning
	}
	m.mu.RUnlock()
	return status, nil
}

func (m *Manager) verifyPrivatePaths(spec service.ServiceSpec, operation string) error {
	checks := []struct {
		path  string
		isDir bool
	}{
		{spec.PMuxPath, false},
		{spec.BinaryPath, false},
		{spec.ConfigPath, false},
		{spec.RuntimeDir, true},
		{spec.LogDir, true},
	}
	for _, check := range checks {
		if err := m.verifyOne(check.path, check.isDir, operation); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) verifyOne(path string, isDir bool, operation string) error {
	if err := m.permissions.VerifySecurePermissions(path, isDir); err != nil {
		return &pmuxerr.Error{
			Code:        pmuxerr.ConfigInsecurePermissions,
			Class:       pmuxerr.Environment,
			Message:     "Windows private permissions could not be verified; the scheduled task operation was blocked (" + operation + ").",
			Explanation: "PMux requires inheritance-disabled DACLs granting access only to the current user and SYSTEM.",
			Evidence:    []string{"path: " + path},
			Repair:      []string{"Run `pmux doctor` to repair and verify private Windows permissions."},
			Cause:       err,
		}
	}
	return nil
}

func (m *Manager) currentSpec() service.ServiceSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneSpec(m.spec)
}

func (m *Manager) clearHealth() {
	m.mu.Lock()
	m.lastHealth = health.Result{}
	m.healthKnown = false
	m.mu.Unlock()
}

func definitionFor(spec service.ServiceSpec) TaskDefinition {
	return TaskDefinition{
		Name:                    service.Identity(service.BackendWindowsTask, spec.InstanceID),
		Description:             OwnershipMarker(spec.InstanceID),
		Author:                  taskAuthor,
		Enabled:                 true,
		LogonTrigger:            true,
		RunOnlyWhenUserLoggedOn: true,
		RunLevel:                RunLevelLeastPrivilege,
		MultipleInstances:       MultipleInstancesIgnoreNew,
		RestartCount:            3,
		RestartInterval:         time.Minute,
		Exec: ExecAction{
			Executable: spec.PMuxPath,
			Arguments: []string{
				"--binary", spec.BinaryPath,
				"--config", spec.ConfigPath,
				"--runtime-dir", spec.RuntimeDir,
				"--log-dir", spec.LogDir,
			},
			WorkingDirectory: spec.RuntimeDir,
		},
	}
}

// OwnershipMarker is persisted in the task registration description and is
// required before PMux mutates or removes a task at the canonical identity.
func OwnershipMarker(instanceID string) string { return ownershipPrefix + instanceID }

func isOwned(task RegisteredTask, instanceID string) bool {
	return task.Definition.Name == service.Identity(service.BackendWindowsTask, instanceID) &&
		task.Definition.Description == OwnershipMarker(instanceID)
}

func mapTaskState(task RegisteredTask) service.ServiceState {
	switch task.State {
	case TaskStateReady, TaskStateDisabled:
		if task.LastResult != 0 {
			return service.ServiceFailed
		}
		return service.ServiceStopped
	case TaskStateQueued:
		return service.ServiceStarting
	case TaskStateRunning:
		return service.ServiceRunning
	default:
		return service.ServiceUnknown
	}
}

func statusDetail(task RegisteredTask) string {
	if task.LastResult == 0 {
		return "Task Scheduler state: " + string(task.State)
	}
	return fmt.Sprintf("Task Scheduler state: %s; last result: 0x%08X", task.State, uint32(task.LastResult))
}

func validateSpec(spec service.ServiceSpec) error {
	if strings.TrimSpace(spec.InstanceID) == "" {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Windows Scheduled Task instance ID must not be empty.")
	}
	expected := service.Identity(service.BackendWindowsTask, spec.InstanceID)
	if spec.Identity != expected {
		return &pmuxerr.Error{
			Code:        pmuxerr.ConfigValidationFailed,
			Class:       pmuxerr.User,
			Message:     "Windows Scheduled Task identity is not canonical.",
			Explanation: "The task identity must be exactly " + expected + ".",
			Evidence:    []string{"identity: " + spec.Identity},
		}
	}
	paths := []struct {
		name string
		path string
	}{
		{"PMux service host", spec.PMuxPath},
		{"CLIProxyAPI binary", spec.BinaryPath},
		{"CLIProxyAPI config", spec.ConfigPath},
		{"runtime directory", spec.RuntimeDir},
		{"log directory", spec.LogDir},
	}
	for _, item := range paths {
		if !isAbsolutePath(item.path) {
			return &pmuxerr.Error{
				Code:        pmuxerr.ConfigPathMismatch,
				Class:       pmuxerr.User,
				Message:     item.name + " path must be absolute.",
				Evidence:    []string{"path: " + item.path},
			}
		}
	}
	return nil
}

func isAbsolutePath(path string) bool {
	if filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func cloneSpec(spec service.ServiceSpec) service.ServiceSpec {
	copy := spec
	copy.Environment = append([]string(nil), spec.Environment...)
	return copy
}

func ownershipError(name, explanation string) error {
	return &pmuxerr.Error{
		Code:        pmuxerr.ServiceForeignOwner,
		Class:       pmuxerr.Environment,
		Message:     "PMux will not modify the Windows Scheduled Task because ownership could not be established.",
		Explanation: explanation,
		Evidence:    []string{"task: " + name},
		Repair:      []string{"Inspect the task in Task Scheduler and complete the explicit adoption/hardening transaction if it belongs to this installation."},
	}
}

func backendError(err error, message string) error {
	return &pmuxerr.Error{
		Code:        pmuxerr.ServiceBackendUnavailable,
		Class:       pmuxerr.Environment,
		Message:     message,
		Explanation: "The Task Scheduler 2.0 COM operation did not complete.",
		Repair:      []string{"Run `pmux doctor` to inspect Windows Task Scheduler availability and service state."},
		Cause:       err,
	}
}
