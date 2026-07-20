package foreground

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)
func TestMain(m *testing.M) {
	for index, argument := range os.Args {
		if argument == "-config" && index+1 < len(os.Args) {
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, os.Interrupt)
			_, _ = io.Copy(os.Stdout, os.Stdin)
			<-signals
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(payload)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}


type fakeChecker struct {
	result health.Result
	err    error
	calls  int
}

func (c *fakeChecker) WaitReady(context.Context) (health.Result, error) {
	c.calls++
	return c.result, c.err
}

type fakeProcess struct {
	mu      sync.Mutex
	done    chan struct{}
	stopped bool
	killed  bool
}

func newFakeProcess() *fakeProcess { return &fakeProcess{done: make(chan struct{})} }
func (*fakeProcess) PID() int      { return 42 }
func (p *fakeProcess) Signal(os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		close(p.done)
	}
	return nil
}
func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.killed = true
	if !p.stopped {
		p.stopped = true
		close(p.done)
	}
	p.mu.Unlock()
	return nil
}
func (p *fakeProcess) Wait() error { <-p.done; return nil }

type fakeRunner struct {
	commands  []Command
	processes []*fakeProcess
	logLine   string
}

func (r *fakeRunner) Start(_ context.Context, command Command) (Process, error) {
	r.commands = append(r.commands, command)
	process := newFakeProcess()
	r.processes = append(r.processes, process)
	if r.logLine != "" {
		_, _ = io.WriteString(command.Stdout, r.logLine)
	}
	return process, nil
}

func testSpec(t *testing.T) service.ServiceSpec {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return service.ServiceSpec{
		InstanceID: "default",
		Identity: service.Identity(service.BackendForeground, "default"),
		PMuxPath: filepath.Join(root, "pmux-service-host"),
		BinaryPath: filepath.Join(root, "cli-proxy-api"),
		ConfigPath: filepath.Join(root, "config.yaml"),
		RuntimeDir: runtimeDir,
		LogDir: filepath.Join(root, "logs"),
		Environment: []string{
			"PATH=/usr/bin", "HOME=/home/user", "PGSTORE_HOST=attacker", "OBJECTSTORE_TOKEN=secret",
			"GITSTORE_URL=foreign", "ANTHROPIC_AUTH_TOKEN=secret", "MANAGEMENT_PASSWORD=secret",
			"HTTP_PROXY=http://inherited.invalid", "SSL_CERT_FILE=/unconfigured/trust.pem", "LANG=C\nINJECTED=1",
		},
	}
}

func TestForegroundLifecycleUsesAbsoluteConfigCleanCWDAndScrubbedEnv(t *testing.T) {
	runner := &fakeRunner{}
	checker := &fakeChecker{result: health.Result{Version: "7.2.92"}}
	manager := New(runner, checker)
	spec := testSpec(t)

	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d", len(runner.commands))
	}
	command := runner.commands[0]
	if command.Path != spec.BinaryPath || !reflect.DeepEqual(command.Args, []string{"-config", spec.ConfigPath}) {
		t.Fatalf("command = %#v", command)
	}
	if command.Dir != spec.RuntimeDir {
		t.Fatalf("cwd = %q", command.Dir)
	}
	if !reflect.DeepEqual(command.Env, []string{"HOME=/home/user", "PATH=/usr/bin"}) {
		t.Fatalf("env = %#v", command.Env)
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != "7.2.92" || status.PID != 42 {
		t.Fatalf("running status = %#v", status)
	}
	status, err = manager.Restart(context.Background())
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(runner.commands) != 2 || status.State != service.ServiceRunning || checker.calls != 2 {
		t.Fatalf("restart commands=%d status=%#v health-calls=%d", len(runner.commands), status, checker.calls)
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	status, _ = manager.Status(context.Background())
	if status.State != service.ServiceNotInstalled {
		t.Fatalf("uninstalled status = %#v", status)
	}
}

func TestForegroundInstallRejectsEnvRuntimeAndOwnershipConflicts(t *testing.T) {
	manager := New(&fakeRunner{}, &fakeChecker{})
	spec := testSpec(t)
	if err := os.WriteFile(filepath.Join(spec.RuntimeDir, ".env"), []byte("PGSTORE_HOST=bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(context.Background(), spec); err == nil {
		t.Fatal("Install accepted runtime .env")
	}
	if err := os.Remove(filepath.Join(spec.RuntimeDir, ".env")); err != nil {
		t.Fatal(err)
	}
	badIdentity := spec
	badIdentity.Identity = "foreign"
	var identityError *pmuxerr.Error
	if err := manager.Install(context.Background(), badIdentity); !errors.As(err, &identityError) || identityError.Code != pmuxerr.ServiceForeignOwner {
		t.Fatalf("identity error = %#v", err)
	}
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	conflict := spec
	conflict.ConfigPath = filepath.Join(filepath.Dir(spec.ConfigPath), "other.yaml")
	err := manager.Install(context.Background(), conflict)
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.ServiceForeignOwner {
		t.Fatalf("conflict error = %#v", err)
	}
}

func TestForegroundLogsAreRedacted(t *testing.T) {
	secret := "sk-1234567890abcdef"
	runner := &fakeRunner{logLine: "Authorization: Bearer top-secret " + secret + " ANTHROPIC_AUTH_TOKEN=another-secret\n"}
	manager := New(runner, &fakeChecker{result: health.Result{Version: health.UnknownVersion, Warning: health.UnknownVersionWarning}})
	if err := manager.Install(context.Background(), testSpec(t)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs, err := manager.Logs(context.Background(), 10, false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(logs)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{"top-secret", secret, "another-secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("logs contain %q: %s", forbidden, text)
		}
	}
	if err := manager.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundPersistentPIDIsValidatedAcrossManagers(t *testing.T) {
	originalInspect, originalStop := inspectProcessOwned, stopRecordedProcess
	defer func() {
		inspectProcessOwned, stopRecordedProcess = originalInspect, originalStop
	}()
	inspectProcessOwned = func(pid int, spec service.ServiceSpec, _ time.Time) bool {
		return pid == 42 && spec.InstanceID == "default"
	}
	stoppedPID := 0
	stopRecordedProcess = func(_ context.Context, pid int, _ time.Duration) error {
		stoppedPID = pid
		return nil
	}
	spec := testSpec(t)
	pidPath := filepath.Join(filepath.Dir(spec.RuntimeDir), "foreground-default.json")
	first := NewPersistent(&fakeRunner{}, &fakeChecker{result: health.Result{Version: "7.2.92"}}, pidPath)
	if err := first.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("PID record: %v", err)
	}
	second := NewPersistent(&fakeRunner{}, &fakeChecker{}, pidPath)
	if err := second.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	status, err := second.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != service.ServiceRunning || status.PID != 42 || status.Healthy {
		t.Fatalf("recovered status = %#v", status)
	}
	if err := second.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if stoppedPID != 42 {
		t.Fatalf("stopped PID = %d", stoppedPID)
	}
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PID record still exists: %v", err)
	}
	if err := first.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestAttachedForegroundStaysUntilAnotherManagerStopsOwnedProcess(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("cross-process console signaling is covered by the Windows adapter tests")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := service.ServiceSpec{
		InstanceID: "attached", Identity: service.Identity(service.BackendForeground, "attached"),
		PMuxPath: binary, BinaryPath: binary, ConfigPath: configPath, RuntimeDir: runtimeDir,
		LogDir: filepath.Join(root, "logs"), Environment: AllowlistedEnvironment(os.Environ()),
	}
	pidPath := filepath.Join(root, "foreground-attached.json")
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	attached := NewAttachedPersistent(OSRunner{}, &fakeChecker{result: health.Result{Version: "7.2.92"}}, pidPath, Streams{
		Stdin: strings.NewReader("attached-input\n"), Stdout: stdout, Stderr: stderr,
	})
	if err := attached.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	wait, err := attached.StartAttached(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- wait(context.Background()) }()
	select {
	case err := <-waited:
		t.Fatalf("attached foreground returned before stop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	inspector := NewPersistent(OSRunner{}, &fakeChecker{}, pidPath)
	if err := inspector.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	status, err := inspector.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != service.ServiceRunning || status.PID <= 0 {
		t.Fatalf("cross-invocation status = %#v", status)
	}
	if err := inspector.Stop(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("attached wait after confirmed stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attached foreground did not return after cross-invocation stop")
	}
	if !strings.Contains(stdout.String(), "attached-input") {
		t.Fatalf("stdout was not attached: %q (stderr %q)", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PID ownership record is stale: %v", err)
	}
}
