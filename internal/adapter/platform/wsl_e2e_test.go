//go:build wsl_e2e && linux

package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/foreground"
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
)

// TestWSLNativeAcceptance runs only on a real WSL runner. The release workflow
// sets PMUX_RELEASE_E2E=1, turning every unavailable native prerequisite into a
// release failure rather than a local skip.
func TestWSLNativeAcceptance(t *testing.T) {
	adapter := newNative("")
	if !adapter.IsWSL() {
		requireWSLPrerequisite(t, "WSL was not detected from WSL_DISTRO_NAME or /proc markers")
	}

	assertWSLRoots(t, adapter)
	assertWSLPrivateModes(t, adapter)
	assertWSLBrowserHandoff(t, adapter)
	assertWSLServiceSelection(t)
	assertWSLForegroundFromArbitraryCWD(t)
}

func assertWSLRoots(t *testing.T, adapter *nativePlatform) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"config": expectedWSLRoot(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")),
		"state":  expectedWSLRoot(os.Getenv("XDG_STATE_HOME"), filepath.Join(home, ".local", "state")),
		"cache":  expectedWSLRoot(os.Getenv("XDG_CACHE_HOME"), filepath.Join(home, ".cache")),
		"data":   expectedWSLRoot(os.Getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share")),
	}
	actual := map[string]func() (string, error){
		"config": adapter.ConfigDir,
		"state":  adapter.StateDir,
		"cache":  adapter.CacheDir,
		"data":   adapter.DataDir,
	}
	for name, resolve := range actual {
		root, err := resolve()
		if err != nil {
			t.Fatalf("resolve %s root: %v", name, err)
		}
		want := filepath.Join(expected[name], "pmux")
		if root != want {
			t.Fatalf("%s root = %q, want %q", name, root, want)
		}
		if !filepath.IsAbs(root) {
			t.Fatalf("%s root is not absolute: %q", name, root)
		}
		if isWindowsMountedWSLPath(root) {
			t.Fatalf("%s root is on a Windows-mounted filesystem: %q", name, root)
		}
	}
}

func expectedWSLRoot(environment, fallback string) string {
	if filepath.IsAbs(environment) {
		return filepath.Clean(environment)
	}
	return fallback
}

func isWindowsMountedWSLPath(path string) bool {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, "/mnt/") || len(clean) < len("/mnt/x") {
		return false
	}
	drive := clean[len("/mnt/")]
	return (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')
}

func assertWSLPrivateModes(t *testing.T, adapter *nativePlatform) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "private")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "secret.json")
	if err := os.WriteFile(file, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SecurePermissions(directory, true); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SecurePermissions(file, false); err != nil {
		t.Fatal(err)
	}
	assertMode(t, directory, 0o700)
	assertMode(t, file, 0o600)
}

func assertWSLBrowserHandoff(t *testing.T, adapter *nativePlatform) {
	t.Helper()
	selected := ""
	adapter.lookPath = func(candidate string) (string, error) {
		if _, err := exec.LookPath(candidate); err != nil {
			return "", err
		}
		selected = candidate
		return "/bin/true", nil
	}
	if err := adapter.OpenBrowser(t.Context(), "https://example.invalid/pmux-wsl-browser-handoff"); err != nil {
		requireWSLPrerequisite(t, "neither wslview nor explorer.exe is available for Windows browser handoff: "+err.Error())
	}
	if selected != "wslview" && selected != "explorer.exe" {
		t.Fatalf("WSL browser adapter selected %q, want wslview or explorer.exe", selected)
	}
}

func assertWSLServiceSelection(t *testing.T) {
	t.Helper()
	want := service.BackendForeground
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		if systemctl, lookupErr := exec.LookPath("systemctl"); lookupErr == nil {
			command := exec.CommandContext(t.Context(), systemctl, "--user", "show-environment")
			if command.Run() == nil {
				want = service.BackendSystemdUser
			}
		}
	}
	if got := DefaultServiceBackend(t.Context()); got != want {
		t.Fatalf("DefaultServiceBackend() = %q, want %q", got, want)
	}
}

type wslReadyChecker struct{}

func (wslReadyChecker) WaitReady(context.Context) (health.Result, error) {
	return health.Result{Version: "unknown", Warning: health.UnknownVersionWarning}, nil
}

func assertWSLForegroundFromArbitraryCWD(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	logDir := filepath.Join(root, "logs")
	stateDir := filepath.Join(root, "state")
	callerDir := filepath.Join(root, "caller")
	for _, directory := range []string{runtimeDir, logDir, stateDir, callerDir} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("host: 127.0.0.1\nport: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observation := filepath.Join(root, "observed.txt")
	fakeCore := filepath.Join(root, "fake-core")
	script := "#!/bin/sh\n" +
		"{ printf 'cwd=%s\\n' \"$PWD\"; printf 'arg1=%s\\narg2=%s\\n' \"$1\" \"$2\"; printf 'pgstore=%s\\n' \"${PGSTORE_HOST-}\"; } > " + shellQuote(observation) + "\n" +
		"trap 'exit 0' INT TERM\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(fakeCore, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	pmuxPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := foreground.NewPersistent(
		foreground.OSRunner{},
		wslReadyChecker{},
		filepath.Join(stateDir, "foreground-wsl-e2e.json"),
	)
	spec := service.ServiceSpec{
		InstanceID: "wsl-e2e",
		PMuxPath:   pmuxPath,
		BinaryPath: fakeCore,
		ConfigPath: configPath,
		RuntimeDir: runtimeDir,
		LogDir:     logDir,
		Environment: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"PGSTORE_HOST=must-not-reach-child",
			"OBJECTSTORE_ENDPOINT=must-not-reach-child",
			"GITSTORE_URL=must-not-reach-child",
		},
	}
	if err := manager.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	t.Chdir(callerDir)
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = manager.Stop(ctx, 5*time.Second)
	})

	body := waitForObservation(t, observation)
	for _, expected := range []string{
		"cwd=" + runtimeDir,
		"arg1=-config",
		"arg2=" + configPath,
		"pgstore=",
	} {
		if !strings.Contains(body, expected+"\n") {
			t.Fatalf("fake foreground core observation does not contain %q:\n%s", expected, body)
		}
	}
	status, err := manager.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Backend != service.BackendForeground || status.State != service.ServiceRunning {
		t.Fatalf("foreground status = backend %q state %q", status.Backend, status.State)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	if err := manager.Stop(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func waitForObservation(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			return string(body)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake foreground core did not write %s", path)
	return ""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func requireWSLPrerequisite(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("PMUX_RELEASE_E2E") == "1" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}
