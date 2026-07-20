//go:build darwin

package launchd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type commandCall struct {
	name string
	args []string
}

type fakeRunner struct {
	loaded       bool
	calls        []commandCall
	stream       []commandCall
	failures     map[string]error
	streamReader io.ReadCloser
	cancelStream bool
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	copied := append([]string(nil), args...)
	f.calls = append(f.calls, commandCall{name: name, args: copied})
	if len(args) == 0 {
		return nil, errors.New("missing operation")
	}
	if err := f.failures[args[0]]; err != nil {
		return nil, err
	}
	switch args[0] {
	case "print":
		if !f.loaded {
			return nil, errors.New("not loaded")
		}
		return []byte("service = {\n\tpid = 4242\n\tenvironment = { SECRET = never-return-this }\n}\n"), nil
	case "bootstrap", "kickstart":
		f.loaded = true
		return nil, nil
	case "bootout":
		f.loaded = false
		return nil, nil
	default:
		return nil, errors.New("unexpected operation")
	}
}

func (f *fakeRunner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	copied := append([]string(nil), args...)
	f.stream = append(f.stream, commandCall{name: name, args: copied})
	if f.streamReader == nil {
		return io.NopCloser(strings.NewReader("logs")), nil
	}
	if f.cancelStream {
		reader := f.streamReader
		go func() {
			<-ctx.Done()
			_ = reader.Close()
		}()
	}
	return f.streamReader, nil
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

func testManager(t *testing.T) (*Manager, *fakeRunner, *fakeHealth, service.ServiceSpec) {
	t.Helper()
	root := t.TempDir()
	runner := &fakeRunner{}
	checker := &fakeHealth{result: health.Result{Version: "7.2.92"}}
	manager, err := New(Config{
		InstanceID: "test-one", PlistDir: filepath.Join(root, "Library", "LaunchAgents"),
		UID: 501, Runner: runner, Health: checker,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	spec := service.ServiceSpec{
		InstanceID: "test-one",
		Identity:   "dev.pmux.cliproxyapi.test-one",
		PMuxPath:   filepath.Join(root, "Application Support", "PMux", "pmux-service-host"),
		BinaryPath: filepath.Join(root, "Application Support", "PMux", "CLIProxyAPI", "cli-proxy-api"),
		ConfigPath: filepath.Join(root, "Application Support", "PMux", "instances", "test-one", "config.yaml"),
		RuntimeDir: filepath.Join(root, "Application Support", "PMux", "instances", "test-one", "runtime"),
		LogDir:     filepath.Join(root, "Application Support", "PMux", "State", "logs"),
		Environment: []string{
			"PATH=/usr/bin:/bin",
			"LANG=en_US.UTF-8",
			"SPECIAL=<safe>&value with spaces",
		},
	}
	return manager, runner, checker, spec
}

func TestInstallRendersCanonicalSafePlist(t *testing.T) {
	manager, _, _, spec := testManager(t)
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	body, err := os.ReadFile(manager.plistPath)
	if err != nil {
		t.Fatalf("ReadFile(plist) error = %v", err)
	}
	root, err := parsePlist(body)
	if err != nil {
		t.Fatalf("parsePlist() error = %v\n%s", err, body)
	}
	if got := stringValue(root["Label"]); got != "dev.pmux.cliproxyapi.test-one" {
		t.Fatalf("Label = %q", got)
	}
	gotArgs, ok := root["ProgramArguments"].([]any)
	if !ok {
		t.Fatalf("ProgramArguments type = %T", root["ProgramArguments"])
	}
	wantArgs := []any{filepath.ToSlash(spec.PMuxPath), "--binary", filepath.ToSlash(spec.BinaryPath), "--config", filepath.ToSlash(spec.ConfigPath)}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("ProgramArguments = %#v, want %#v", gotArgs, wantArgs)
	}
	if got := stringValue(root["WorkingDirectory"]); got != filepath.ToSlash(spec.RuntimeDir) {
		t.Fatalf("WorkingDirectory = %q", got)
	}
	if got := stringValue(root["StandardOutPath"]); got != filepath.Join(filepath.ToSlash(spec.LogDir), "test-one.out.log") {
		t.Fatalf("StandardOutPath = %q", got)
	}
	if got := stringValue(root["StandardErrorPath"]); got != filepath.Join(filepath.ToSlash(spec.LogDir), "test-one.err.log") {
		t.Fatalf("StandardErrorPath = %q", got)
	}
	if got, ok := root["RunAtLoad"].(bool); !ok || !got {
		t.Fatalf("RunAtLoad = %#v", root["RunAtLoad"])
	}
	keepAlive, ok := root["KeepAlive"].(map[string]any)
	if !ok || keepAlive["SuccessfulExit"] != false {
		t.Fatalf("KeepAlive = %#v", root["KeepAlive"])
	}
	env, ok := root["EnvironmentVariables"].(map[string]any)
	if !ok {
		t.Fatalf("EnvironmentVariables = %T", root["EnvironmentVariables"])
	}
	for key, want := range map[string]string{
		"PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8",
		"SPECIAL":              "<safe>&value with spaces",
		ownerEnvironmentKey:    ownerEnvironmentVal,
		instanceEnvironmentKey: "test-one",
	} {
		if got := stringValue(env[key]); got != want {
			t.Errorf("EnvironmentVariables[%q] = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(string(body), "&value with spaces</string>") {
		t.Fatal("plist environment value was not XML escaped")
	}
	if mode := fileMode(t, manager.plistPath); mode != 0o600 {
		t.Fatalf("plist mode = %#o, want 0600", mode)
	}
	if mode := fileMode(t, filepath.ToSlash(spec.RuntimeDir)); mode != 0o700 {
		t.Fatalf("runtime mode = %#o, want 0700", mode)
	}
	if mode := fileMode(t, filepath.ToSlash(spec.LogDir)); mode != 0o700 {
		t.Fatalf("log mode = %#o, want 0700", mode)
	}
}

func TestInstallRejectsForbiddenEnvironmentAndRuntimeDotEnv(t *testing.T) {
	manager, _, _, spec := testManager(t)
	for _, variable := range []string{
		"PGSTORE_DSN=postgres://example", "OBJECTSTORE_ENDPOINT=http://example",
		"GITSTORE_TOKEN=secret", "MANAGEMENT_PASSWORD=secret",
	} {
		t.Run(strings.SplitN(variable, "=", 2)[0], func(t *testing.T) {
			candidate := spec
			candidate.Environment = []string{variable}
			err := manager.Install(context.Background(), candidate)
			assertPMuxCode(t, err, pmuxerr.ConfigValidationFailed)
		})
	}
	if err := os.MkdirAll(filepath.ToSlash(spec.RuntimeDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.ToSlash(spec.RuntimeDir), ".env"), []byte("PGSTORE_DSN=bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertPMuxCode(t, manager.Install(context.Background(), spec), pmuxerr.ConfigValidationFailed)
}

func TestForeignPlistIsNeverOverwrittenOrRemoved(t *testing.T) {
	manager, runner, _, spec := testManager(t)
	if err := os.MkdirAll(filepath.Dir(manager.plistPath), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>Label</key><string>dev.pmux.cliproxyapi.test-one</string></dict></plist>`)
	if err := os.WriteFile(manager.plistPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	assertPMuxCode(t, manager.Install(context.Background(), spec), pmuxerr.ServiceForeignOwner)
	assertPMuxCode(t, manager.Uninstall(context.Background()), pmuxerr.ServiceForeignOwner)
	got, err := os.ReadFile(manager.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, foreign) {
		t.Fatalf("foreign plist changed: %q", got)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("launchctl invoked for foreign plist: %#v", runner.calls)
	}
}
func TestUninstallRetainsOwnedPlistWhenBootoutFails(t *testing.T) {
	manager, runner, _, spec := testManager(t)
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runner.loaded = true
	runner.failures = map[string]error{"bootout": errors.New("launchctl refused")}

	assertPMuxCode(t, manager.Uninstall(context.Background()), pmuxerr.ServiceStartFailed)
	if _, err := os.Stat(manager.plistPath); err != nil {
		t.Fatalf("owned plist was removed after bootout failure: %v", err)
	}
}

func TestLifecycleTransitionsAndLaunchctlArgumentBoundaries(t *testing.T) {
	manager, runner, checker, spec := testManager(t)
	ctx := context.Background()

	status, err := manager.Status(ctx)
	if err != nil || status.State != service.ServiceNotInstalled {
		t.Fatalf("initial Status() = %#v, %v", status, err)
	}
	if err := manager.Install(ctx, spec); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceStopped {
		t.Fatalf("installed Status() = %#v, %v", status, err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if checker.calls != 1 {
		t.Fatalf("health calls after Start = %d", checker.calls)
	}
	assertCall(t, runner.calls, commandCall{
		name: "/bin/launchctl",
		args: []string{"bootstrap", "gui/501", manager.plistPath},
	})
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceRunning || status.PID != 4242 {
		t.Fatalf("running Status() = %#v, %v", status, err)
	}
	if strings.Contains(status.Detail, "SECRET") || status.Detail != "loaded" {
		t.Fatalf("Status.Detail leaked launchctl output: %q", status.Detail)
	}

	restarted, err := manager.Restart(ctx)
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if !restarted.Healthy || restarted.CoreVersion != "7.2.92" || checker.calls != 2 {
		t.Fatalf("Restart() status = %#v, health calls = %d", restarted, checker.calls)
	}
	assertCall(t, runner.calls, commandCall{
		name: "/bin/launchctl",
		args: []string{"kickstart", "-k", "gui/501/dev.pmux.cliproxyapi.test-one"},
	})

	if err := manager.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	assertCall(t, runner.calls, commandCall{
		name: "/bin/launchctl",
		args: []string{"bootout", "gui/501/dev.pmux.cliproxyapi.test-one"},
	})
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceStopped {
		t.Fatalf("stopped Status() = %#v, %v", status, err)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceNotInstalled {
		t.Fatalf("uninstalled Status() = %#v, %v", status, err)
	}
}

func TestLogsUsesSeparateTailArgumentsAndOwnedPaths(t *testing.T) {
	manager, runner, _, spec := testManager(t)
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	reader, err := manager.Logs(context.Background(), 37, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if len(runner.stream) != 1 {
		t.Fatalf("stream calls = %d", len(runner.stream))
	}
	want := commandCall{name: "/usr/bin/tail", args: []string{
		"-n", "37", "-F", filepath.Join(filepath.ToSlash(spec.LogDir), "test-one.out.log"), filepath.Join(filepath.ToSlash(spec.LogDir), "test-one.err.log"),
	}}
	if !reflect.DeepEqual(runner.stream[0], want) {
		t.Fatalf("stream call = %#v, want %#v", runner.stream[0], want)
	}
}

func TestLogsRedactSecretsAndTerminalControls(t *testing.T) {
	manager, runner, _, spec := testManager(t)
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	secrets := []string{
		"complete-bearer-secret",
		"complete-management-secret",
		"complete-api-key-secret",
		"sk-completeproxysecret",
	}
	runner.streamReader = io.NopCloser(strings.NewReader(
		"ordinary launchd log text\n" +
			"Authorization:\x1b[31m Bearer " + secrets[0] +
			" X-Management-Key: " + secrets[1] +
			" api_key=" + secrets[2] +
			" proxy=" + secrets[3] +
			" \x1b]52;c;clipboard-payload\a colored\x1b[0m\r\n",
	))

	reader, err := manager.Logs(context.Background(), 25, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("LaunchAgent logs disclosed %q: %q", secret, text)
		}
	}
	for _, character := range text {
		if character != '\n' && unicode.IsControl(character) {
			t.Fatalf("LaunchAgent logs retained terminal control %U: %q", character, text)
		}
	}
	for _, ordinary := range []string{"ordinary launchd log text", "colored"} {
		if !strings.Contains(text, ordinary) {
			t.Fatalf("LaunchAgent logs dropped ordinary text %q: %q", ordinary, text)
		}
	}
	if strings.Contains(text, "clipboard-payload") {
		t.Fatalf("LaunchAgent logs retained an OSC terminal-control payload: %q", text)
	}
}

func TestLogsFollowStreamsUntilContextCancellation(t *testing.T) {
	manager, runner, _, spec := testManager(t)
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	sourceReader, sourceWriter := io.Pipe()
	runner.streamReader = sourceReader
	runner.cancelStream = true
	ctx, cancel := context.WithCancel(context.Background())
	logs, err := manager.Logs(ctx, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	buffered := bufio.NewReader(logs)

	writeLine := func(line string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, writeErr := io.WriteString(sourceWriter, line)
			done <- writeErr
		}()
		return done
	}
	waitForWrite := func(done <-chan error) {
		t.Helper()
		select {
		case writeErr := <-done:
			if writeErr != nil {
				t.Fatalf("source write failed: %v", writeErr)
			}
		case <-time.After(time.Second):
			t.Fatal("source write blocked; follow stream stopped consuming")
		}
	}

	firstWrite := writeLine("first ordinary line\n")
	first, err := buffered.ReadString('\n')
	if err != nil || first != "first ordinary line\n" {
		t.Fatalf("first followed line = %q, %v", first, err)
	}
	waitForWrite(firstWrite)
	secondWrite := writeLine("second ordinary line\n")
	second, err := buffered.ReadString('\n')
	if err != nil || second != "second ordinary line\n" {
		t.Fatalf("second followed line = %q, %v", second, err)
	}
	waitForWrite(secondWrite)

	cancel()
	readDone := make(chan error, 1)
	go func() {
		_, readErr := buffered.ReadString('\n')
		readDone <- readErr
	}()
	select {
	case readErr := <-readDone:
		if readErr == nil {
			t.Fatal("follow stream remained open after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("follow stream did not unblock after context cancellation")
	}
}

func TestLogsBoundIndividualLineBuffering(t *testing.T) {
	manager, runner, _, spec := testManager(t)
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	runner.streamReader = io.NopCloser(strings.NewReader(strings.Repeat("x", maxLogLineBytes+1)))
	logs, err := manager.Logs(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	if _, err := io.ReadAll(logs); err == nil {
		t.Fatalf("LaunchAgent log reader accepted a line larger than %d bytes", maxLogLineBytes)
	}
}

func TestHealthFailureUsesCanonicalCondition(t *testing.T) {
	manager, _, checker, spec := testManager(t)
	checker.err = errors.New("deadline")
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	assertPMuxCode(t, manager.Start(context.Background()), pmuxerr.ServiceHealthDeadline)
}

func assertCall(t *testing.T, calls []commandCall, want commandCall) {
	t.Helper()
	for _, call := range calls {
		if reflect.DeepEqual(call, want) {
			return
		}
	}
	t.Fatalf("missing command call %#v in %#v", want, calls)
}

func assertPMuxCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected PMux error %s", code)
	}
	var target *pmuxerr.Error
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *pmuxerr.Error", err)
	}
	if target.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", target.Code, code, err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
