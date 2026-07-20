package systemd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type fakeRunner struct {
	calls  []string
	status string
	logs   string
}

func (r *fakeRunner) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, executable+" "+strings.Join(args, " "))
	for _, arg := range args {
		if arg == "show" {
			return []byte(r.status), nil
		}
	}
	return nil, nil
}
func (r *fakeRunner) Stream(_ context.Context, executable string, args ...string) (io.ReadCloser, error) {
	r.calls = append(r.calls, executable+" "+strings.Join(args, " "))
	return io.NopCloser(strings.NewReader(r.logs)), nil
}

type fakeChecker struct {
	result health.Result
	calls  int
}

func (c *fakeChecker) WaitReady(context.Context) (health.Result, error) {
	c.calls++
	return c.result, nil
}

func testSpec(t *testing.T) service.ServiceSpec {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return service.ServiceSpec{
		InstanceID:  "default",
		Identity:    service.Identity(service.BackendSystemdUser, "default"),
		PMuxPath:    filepath.Join(root, "pmux-service-host"),
		BinaryPath:  filepath.Join(root, "cli-proxy-api"),
		ConfigPath:  filepath.Join(root, "config.yaml"),
		RuntimeDir:  runtimeDir,
		LogDir:      filepath.Join(root, "logs"),
		Environment: []string{"PATH=/usr/bin", "HOME=/home/u", "PGSTORE_HOST=bad", "ANTHROPIC_AUTH_TOKEN=secret"},
	}
}

func TestRenderUnitUsesCanonicalIdentityArgvCWDAndEnvironment(t *testing.T) {
	spec := testSpec(t)
	body, err := RenderUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		OwnershipMarker,
		"# PMux-Instance: default",
		"WorkingDirectory=\"" + spec.RuntimeDir + "\"",
		"ExecStart=\"" + spec.PMuxPath + "\" --binary \"" + spec.BinaryPath + "\" --config \"" + spec.ConfigPath + "\" --runtime-dir \"" + spec.RuntimeDir + "\"",
		"Environment=\"HOME=/home/u\"",
		"Environment=\"PATH=/usr/bin\"",
		"WantedBy=default.target",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("unit missing %q:\n%s", expected, text)
		}
	}
	for _, forbidden := range []string{"PGSTORE", "OBJECTSTORE", "GITSTORE", "ANTHROPIC_AUTH_TOKEN", "secret"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("unit contains forbidden %q:\n%s", forbidden, text)
		}
	}
	if !Owned(body, "default") {
		t.Fatal("rendered unit is not recognized as PMux-owned")
	}
}

func TestSystemdLifecycleIsSymmetricAndHealthHeaderIsOptional(t *testing.T) {
	spec := testSpec(t)
	unitDir := filepath.Join(t.TempDir(), "units")
	runner := &fakeRunner{status: "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=123\nExecMainStatus=0\n"}
	checker := &fakeChecker{result: health.Result{Version: health.UnknownVersion, Warning: health.UnknownVersionWarning}}
	manager := New("default", unitDir, runner, checker)

	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := manager.Restart(context.Background())
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != health.UnknownVersion || status.Warning != health.UnknownVersionWarning {
		t.Fatalf("status = %#v", status)
	}
	if checker.calls != 2 {
		t.Fatalf("health checks = %d, want 2", checker.calls)
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, spec.Identity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit remains after uninstall: %v", err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{"systemctl --user daemon-reload", "systemctl --user start " + spec.Identity, "systemctl --user restart " + spec.Identity, "systemctl --user stop " + spec.Identity, "systemctl --user disable " + spec.Identity} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing lifecycle call %q:\n%s", expected, joined)
		}
	}
}

func TestInstallRejectsForeignUnitAndRuntimeDotEnv(t *testing.T) {
	spec := testSpec(t)
	unitDir := t.TempDir()
	unitPath := filepath.Join(unitDir, spec.Identity)
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart=/foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New("default", unitDir, &fakeRunner{}, &fakeChecker{})
	err := manager.Install(context.Background(), spec)
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.ServiceForeignOwner {
		t.Fatalf("foreign error = %#v", err)
	}
	if err := os.Remove(unitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.RuntimeDir, ".env"), []byte("GITSTORE_URL=bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(context.Background(), spec); err == nil {
		t.Fatal("Install accepted runtime .env")
	}
}

func TestSystemdLogsAreRedacted(t *testing.T) {
	spec := testSpec(t)
	unitDir := t.TempDir()
	secret := "sk-1234567890abcdef"
	runner := &fakeRunner{logs: "Authorization: Bearer token-value " + secret + "\n"}
	manager := New("default", unitDir, runner, &fakeChecker{})
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	logs, err := manager.Logs(context.Background(), 20, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	body, err := io.ReadAll(logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token-value", secret} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("logs contain %q: %s", forbidden, body)
		}
	}
}

func TestStopHonorsCallerTimeout(t *testing.T) {
	spec := testSpec(t)
	unitDir := t.TempDir()
	runner := &fakeRunner{}
	manager := New("default", unitDir, runner, &fakeChecker{})
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}
