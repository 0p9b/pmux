package windowstask

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestInstallRegistersCanonicalCOMExecAction(t *testing.T) {
	spec := testSpec()
	com := newFakeCOM()
	permissions := &fakePermissions{}
	manager := newTestManager(t, spec, com, permissions, &fakeLogs{}, &fakeHealth{})

	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if com.registerCalls != 1 {
		t.Fatalf("RegisterTaskDefinition calls = %d, want 1", com.registerCalls)
	}
	definition := com.task.Definition
	wantName := "PMux CLIProxyAPI (alpha)"
	if definition.Name != wantName {
		t.Fatalf("task name = %q, want %q", definition.Name, wantName)
	}
	if definition.Name != service.Identity(service.BackendWindowsTask, spec.InstanceID) {
		t.Fatal("registered task did not use the canonical domain identity")
	}
	if definition.Description != OwnershipMarker(spec.InstanceID) {
		t.Fatalf("description = %q, want ownership marker", definition.Description)
	}
	if definition.Exec.Executable != spec.PMuxPath {
		t.Fatalf("ExecAction executable = %q, want %q", definition.Exec.Executable, spec.PMuxPath)
	}
	wantArgs := []string{
		"--binary", spec.BinaryPath,
		"--config", spec.ConfigPath,
		"--runtime-dir", spec.RuntimeDir,
		"--log-dir", spec.LogDir,
	}
	if !reflect.DeepEqual(definition.Exec.Arguments, wantArgs) {
		t.Fatalf("ExecAction arguments = %#v, want %#v", definition.Exec.Arguments, wantArgs)
	}
	if definition.Exec.WorkingDirectory != spec.RuntimeDir {
		t.Fatalf("ExecAction working directory = %q, want %q", definition.Exec.WorkingDirectory, spec.RuntimeDir)
	}
	if !definition.LogonTrigger || !definition.RunOnlyWhenUserLoggedOn || definition.RunLevel != RunLevelLeastPrivilege {
		t.Fatalf("task principal/trigger is not current-user on-logon least-privilege: %#v", definition)
	}
	if definition.MultipleInstances != MultipleInstancesIgnoreNew {
		t.Fatalf("multiple-instance policy = %q, want %q", definition.MultipleInstances, MultipleInstancesIgnoreNew)
	}
	if strings.Contains(strings.ToLower(definition.Exec.Executable), "schtasks") {
		t.Fatal("ExecAction executable must never be schtasks")
	}
	for _, argument := range definition.Exec.Arguments {
		if strings.Contains(strings.ToLower(argument), "/tr") {
			t.Fatalf("ExecAction arguments contain forbidden /TR command-string field: %q", argument)
		}
	}

	wantPermissionChecks := []permissionCheck{
		{spec.PMuxPath, false},
		{spec.BinaryPath, false},
		{spec.ConfigPath, false},
		{spec.RuntimeDir, true},
		{spec.LogDir, true},
	}
	if !reflect.DeepEqual(permissions.checks, wantPermissionChecks) {
		t.Fatalf("DACL checks = %#v, want %#v", permissions.checks, wantPermissionChecks)
	}
}

func TestDACLFailureBlocksInstallAndStart(t *testing.T) {
	t.Run("install", func(t *testing.T) {
		spec := testSpec()
		com := newFakeCOM()
		permissions := &fakePermissions{failPath: spec.ConfigPath, err: errors.New("unexpected Users ACE")}
		manager := newTestManager(t, spec, com, permissions, &fakeLogs{}, &fakeHealth{})

		err := manager.Install(context.Background(), spec)
		assertPMuxCode(t, err, pmuxerr.ConfigInsecurePermissions)
		if com.registerCalls != 0 {
			t.Fatalf("RegisterTaskDefinition calls = %d, want 0", com.registerCalls)
		}
	})

	t.Run("start", func(t *testing.T) {
		spec := testSpec()
		com := newFakeCOM()
		permissions := &fakePermissions{}
		healthCheck := &fakeHealth{}
		manager := newTestManager(t, spec, com, permissions, &fakeLogs{}, healthCheck)
		if err := manager.Install(context.Background(), spec); err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		permissions.failPath = spec.BinaryPath
		permissions.err = errors.New("DACL inheritance enabled")

		err := manager.Start(context.Background())
		assertPMuxCode(t, err, pmuxerr.ConfigInsecurePermissions)
		if com.runCalls != 0 {
			t.Fatalf("RunTask calls = %d, want 0", com.runCalls)
		}
		if healthCheck.calls != 0 {
			t.Fatalf("health checks = %d, want 0", healthCheck.calls)
		}
	})
}

func TestForeignTaskOwnershipIsRefused(t *testing.T) {
	spec := testSpec()
	com := newFakeCOM()
	com.task = RegisteredTask{
		Definition: TaskDefinition{Name: spec.Identity, Description: "created by another application"},
		State:      TaskStateReady,
	}
	com.registered = true
	permissions := &fakePermissions{}
	manager := newTestManager(t, spec, com, permissions, &fakeLogs{}, &fakeHealth{})

	err := manager.Install(context.Background(), spec)
	assertPMuxCode(t, err, pmuxerr.ServiceForeignOwner)
	if com.registerCalls != 0 {
		t.Fatalf("foreign task was overwritten; register calls = %d", com.registerCalls)
	}

	err = manager.Start(context.Background())
	assertPMuxCode(t, err, pmuxerr.ServiceForeignOwner)
	if com.runCalls != 0 {
		t.Fatalf("foreign task was started; run calls = %d", com.runCalls)
	}

	err = manager.Uninstall(context.Background())
	assertPMuxCode(t, err, pmuxerr.ServiceForeignOwner)
	if com.deleteCalls != 0 {
		t.Fatalf("foreign task was deleted; delete calls = %d", com.deleteCalls)
	}
}

func TestLifecycleIsSymmetricThroughCOMAndUsesPMuxLogs(t *testing.T) {
	spec := testSpec()
	com := newFakeCOM()
	permissions := &fakePermissions{}
	logs := &fakeLogs{body: "one\ntwo\n"}
	healthCheck := &fakeHealth{result: health.Result{Version: "7.2.92"}}
	manager := newTestManager(t, spec, com, permissions, logs, healthCheck)
	ctx := context.Background()

	if err := manager.Install(ctx, spec); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != service.ServiceStopped {
		t.Fatalf("state after install = %q, want stopped", status.State)
	}

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil {
		t.Fatalf("Status() after start error = %v", err)
	}
	if status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != "7.2.92" {
		t.Fatalf("status after start = %#v, want running healthy 7.2.92", status)
	}

	status, err = manager.Restart(ctx)
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if status.State != service.ServiceRunning || !status.Healthy {
		t.Fatalf("status after restart = %#v, want running and healthy", status)
	}
	if com.stopCalls != 1 || com.runCalls != 2 || healthCheck.calls != 2 {
		t.Fatalf("restart lifecycle counts: stop=%d run=%d health=%d; want 1,2,2", com.stopCalls, com.runCalls, healthCheck.calls)
	}

	reader, err := manager.Logs(ctx, 25, true)
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	defer func() { _ = reader.Close() }()
	contents, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(contents) != logs.body {
		t.Fatalf("log contents = %q, want %q", contents, logs.body)
	}
	if logs.instanceID != spec.InstanceID || logs.logDir != spec.LogDir || logs.tail != 25 || !logs.follow {
		t.Fatalf("PMux log request = %#v", logs)
	}

	if err := manager.Stop(ctx, 9*time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if com.lastStopTimeout != 9*time.Second {
		t.Fatalf("stop timeout = %s, want 9s", com.lastStopTimeout)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if com.deleteCalls != 1 || com.registered {
		t.Fatalf("uninstall did not symmetrically remove task: deletes=%d registered=%t", com.deleteCalls, com.registered)
	}
	status, err = manager.Status(ctx)
	if err != nil {
		t.Fatalf("Status() after uninstall error = %v", err)
	}
	if status.State != service.ServiceNotInstalled {
		t.Fatalf("state after uninstall = %q, want not-installed", status.State)
	}
}

func TestNonCanonicalIdentityIsRejected(t *testing.T) {
	spec := testSpec()
	spec.Identity = "not-canonical"
	_, err := New(spec, newFakeCOM(), &fakePermissions{}, &fakeLogs{}, &fakeHealth{})
	assertPMuxCode(t, err, pmuxerr.ConfigValidationFailed)
}

func testSpec() service.ServiceSpec {
	return service.ServiceSpec{
		InstanceID: "alpha",
		Identity:   service.Identity(service.BackendWindowsTask, "alpha"),
		PMuxPath:   `C:\Program Files\PMux\pmux-service-host.exe`,
		BinaryPath: `C:\Users\alice\AppData\Local\PMux\Data\cli-proxy-api\current\cli-proxy-api.exe`,
		ConfigPath: `C:\Users\alice\AppData\Local\PMux\Data\instances\alpha\config.yaml`,
		RuntimeDir: `C:\Users\alice\AppData\Local\PMux\Data\instances\alpha\runtime`,
		LogDir:     `C:\Users\alice\AppData\Local\PMux\State\logs`,
	}
}

func newTestManager(t *testing.T, spec service.ServiceSpec, com COM, permissions PermissionVerifier, logs LogReader, healthCheck health.Checker) *Manager {
	t.Helper()
	manager, err := New(spec, com, permissions, logs, healthCheck)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func assertPMuxCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want PMux code %s", code)
	}
	var pe *pmuxerr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *pmuxerr.Error: %v", err, err)
	}
	if pe.Code != code {
		t.Fatalf("PMux code = %q, want %q", pe.Code, code)
	}
}

type fakeCOM struct {
	task            RegisteredTask
	registered      bool
	registerCalls   int
	runCalls        int
	stopCalls       int
	deleteCalls     int
	lastStopTimeout time.Duration
}

func newFakeCOM() *fakeCOM { return &fakeCOM{} }

func (f *fakeCOM) GetTask(context.Context, string) (RegisteredTask, error) {
	if !f.registered {
		return RegisteredTask{}, ErrTaskNotFound
	}
	return f.task, nil
}

func (f *fakeCOM) RegisterTaskDefinition(_ context.Context, definition TaskDefinition) error {
	f.registerCalls++
	f.task = RegisteredTask{Definition: definition, State: TaskStateReady}
	f.registered = true
	return nil
}

func (f *fakeCOM) DeleteTask(context.Context, string) error {
	if !f.registered {
		return ErrTaskNotFound
	}
	f.deleteCalls++
	f.task = RegisteredTask{}
	f.registered = false
	return nil
}

func (f *fakeCOM) RunTask(context.Context, string) error {
	f.runCalls++
	f.task.State = TaskStateRunning
	f.task.PID = 4242
	f.task.Since = time.Unix(1_700_000_000, 0)
	return nil
}

func (f *fakeCOM) StopTask(_ context.Context, _ string, timeout time.Duration) error {
	f.stopCalls++
	f.lastStopTimeout = timeout
	f.task.State = TaskStateReady
	f.task.PID = 0
	f.task.Since = time.Time{}
	return nil
}

type permissionCheck struct {
	path  string
	isDir bool
}

type fakePermissions struct {
	checks   []permissionCheck
	failPath string
	err      error
}

func (f *fakePermissions) VerifySecurePermissions(path string, isDir bool) error {
	f.checks = append(f.checks, permissionCheck{path: path, isDir: isDir})
	if path == f.failPath {
		return f.err
	}
	return nil
}

type fakeHealth struct {
	result health.Result
	err    error
	calls  int
}

func (f *fakeHealth) WaitReady(context.Context) (health.Result, error) {
	f.calls++
	return f.result, f.err
}

type fakeLogs struct {
	body       string
	instanceID string
	logDir     string
	tail       int
	follow     bool
}

func (f *fakeLogs) Open(_ context.Context, instanceID, logDir string, tail int, follow bool) (io.ReadCloser, error) {
	f.instanceID = instanceID
	f.logDir = logDir
	f.tail = tail
	f.follow = follow
	return io.NopCloser(strings.NewReader(f.body)), nil
}
