//go:build darwin && platform_e2e

package launchd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
)

const releaseE2EEnvironment = "PMUX_RELEASE_E2E"

const launchdE2EEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>com.apple.security.network.client</key><true/>
  <key>com.apple.security.network.server</key><true/>
</dict></plist>`

const launchdFakeCoreSource = `package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

type config struct {
	Address string ` + "`json:\"address\"`" + `
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "-config" {
		panic("usage: fake-core -config /absolute/path")
	}
	body, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	var cfg config
	if err := json.Unmarshal(body, &cfg); err != nil {
		panic(err)
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-CPA-VERSION", "e2e-core")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	fmt.Fprintln(os.Stdout, "fake core started")
	if err := http.ListenAndServe(cfg.Address, nil); err != nil {
		panic(err)
	}
}
`

type e2eCoreConfig struct {
	Address string `json:"address"`
}

func TestLaunchAgentReleaseE2E(t *testing.T) {
	if os.Getenv(releaseE2EEnvironment) != "1" {
		t.Skip("set PMUX_RELEASE_E2E=1 on a macOS release runner to exercise launchctl")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root := t.TempDir()
	repoRoot := repositoryRootFromCaller(t)
	home := mustHome(t)
	stableRoot := filepath.Join(home, "Library", "Application Support", "pmux-e2e", strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(stableRoot, 0o700); err != nil {
		t.Fatalf("prepare stable e2e root: %v", err)
	}
	instanceID := "release-e2e-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	label := service.Identity(service.BackendLaunchd, instanceID)
	address := reserveAddress(t)

	hostPath := filepath.Join(stableRoot, "pmux-service-host")
	corePath := filepath.Join(stableRoot, "fake-core")
	buildGoBinary(t, ctx, repoRoot, hostPath, "./cmd/pmux")
	fakeSource := filepath.Join(stableRoot, "fake-core.go")
	if err := os.WriteFile(fakeSource, []byte(launchdFakeCoreSource), 0o600); err != nil {
		t.Fatalf("write fake core source: %v", err)
	}
	buildGoBinary(t, ctx, root, corePath, fakeSource)
	adHocSign(t, hostPath)
	adHocSign(t, corePath)

	// Keep every path the plist references off t.TempDir(): /var is a symlink
	// to /private/var and launchd rejects jobs whose referenced paths resolve
	// through symlinks (bootstrap fails with I/O error 5).
	configPath := filepath.Join(stableRoot, "instance", "config.json")
	writeCoreConfig(t, configPath, e2eCoreConfig{Address: address})
	t.Cleanup(func() { _ = os.RemoveAll(stableRoot) })

	runtimeDir := filepath.Join(stableRoot, "instance", "runtime")
	logDir := filepath.Join(stableRoot, "state", "logs")
	for _, dir := range []string{runtimeDir, logDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o700); err != nil {
		t.Fatalf("prepare LaunchAgents directory: %v", err)
	}
	poller := health.NewPoller(health.HTTPProbe{BaseURL: "http://" + address})
	manager, err := New(Config{
		InstanceID: instanceID,
		PlistDir:   plistDir,
		UID:        os.Getuid(),
		Health:     poller,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	spec := service.ServiceSpec{
		InstanceID: instanceID,
		Identity:   label,
		PMuxPath:   hostPath,
		BinaryPath: corePath,
		ConfigPath: configPath,
		RuntimeDir: runtimeDir,
		LogDir:     logDir,
		Environment: []string{
			"PATH=/usr/bin:/bin",
			"HOME=" + home,
			"TMPDIR=" + os.TempDir(),
		},
	}

	installed := false
	defer func() {
		if !installed {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = manager.Stop(cleanupCtx, 5*time.Second)
		_ = manager.Uninstall(cleanupCtx)
	}()

	if err := manager.Install(ctx, spec); err != nil {
		t.Fatalf("Install(): %v", err)
	}
	installed = true
	assertReleasePlist(t, manager.plistPath, spec, label)

	if err := manager.Start(ctx); err != nil {
		dumpLaunchdDiagnostics(t, manager, label)
		t.Fatalf("Start(): %v", err)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatalf("Status() after start: %v", err)
	}
	if status.State != service.ServiceRunning || status.PID <= 0 {
		t.Fatalf("Status() after start = %#v", status)
	}

	restarted, err := manager.Restart(ctx)
	if err != nil {
		t.Fatalf("Restart(): %v", err)
	}
	if restarted.State != service.ServiceRunning || !restarted.Healthy || restarted.CoreVersion != "e2e-core" {
		t.Fatalf("Restart() = %#v", restarted)
	}

	logs, err := manager.Logs(ctx, 50, false)
	if err != nil {
		t.Fatalf("Logs(): %v", err)
	}
	logBody, readErr := io.ReadAll(logs)
	closeErr := logs.Close()
	if readErr != nil {
		t.Fatalf("read logs: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close logs: %v", closeErr)
	}
	if !containsBytes(logBody, []byte("fake core started")) {
		t.Fatalf("logs did not contain fake core lifecycle evidence: %q", logBody)
	}

	if err := manager.Stop(ctx, 5*time.Second); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil {
		t.Fatalf("Status() after stop: %v", err)
	}
	if status.State != service.ServiceStopped {
		t.Fatalf("Status() after stop = %#v", status)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall(): %v", err)
	}
	installed = false
	if _, err := os.Stat(manager.plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remains after uninstall: %v", err)
	}
}

func assertReleasePlist(t *testing.T, path string, spec service.ServiceSpec, label string) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("plist path is not absolute: %q", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	owned, err := isOwnedPlist(body, label, spec.InstanceID)
	if err != nil || !owned {
		t.Fatalf("plist ownership = %v, %v", owned, err)
	}
	root, err := parsePlist(body)
	if err != nil {
		t.Fatalf("parse plist: %v", err)
	}
	wantArgs := []any{
		spec.PMuxPath, "--binary", spec.BinaryPath, "--config", spec.ConfigPath,
		"--runtime-dir", spec.RuntimeDir, "--log-dir", spec.LogDir,
	}
	if got := root["ProgramArguments"]; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("ProgramArguments = %#v, want %#v", got, wantArgs)
	}
	for name, value := range map[string]string{
		"PMuxPath": spec.PMuxPath, "BinaryPath": spec.BinaryPath,
		"ConfigPath": spec.ConfigPath, "RuntimeDir": spec.RuntimeDir,
	} {
		if !filepath.IsAbs(value) {
			t.Fatalf("%s is not absolute: %q", name, value)
		}
	}
	if got := stringValue(root["WorkingDirectory"]); got != spec.RuntimeDir {
		t.Fatalf("WorkingDirectory = %q, want %q", got, spec.RuntimeDir)
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	return address
}

func buildGoBinary(t *testing.T, ctx context.Context, workingDirectory, output string, packageOrFile ...string) {
	t.Helper()
	if len(packageOrFile) == 0 {
		t.Fatal("buildGoBinary requires a package or file")
	}
	args := []string{"build", "-trimpath", "-o", output}
	args = append(args, packageOrFile...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = workingDirectory
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build %v: %v\n%s", packageOrFile, err, out)
	}
}

func adHocSign(t *testing.T, path string) {
	t.Helper()
	entitlements := filepath.Join(t.TempDir(), "entitlements.plist")
	if err := os.WriteFile(entitlements, []byte(launchdE2EEntitlements), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("codesign", "-s", "-", "-f", "--entitlements", entitlements, path).CombinedOutput()
	if err != nil {
		t.Fatalf("codesign %s: %v\n%s", path, err, out)
	}
}

func writeCoreConfig(t *testing.T, path string, cfg e2eCoreConfig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func repositoryRootFromCaller(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate repository source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", ".."))
}

func dumpLaunchdDiagnostics(t *testing.T, manager *Manager, label string) {
	t.Helper()
	if body, err := os.ReadFile(manager.plistPath); err == nil {
		t.Logf("plist body:\n%s", body)
	}
	if out, err := exec.Command("plutil", "-lint", manager.plistPath).CombinedOutput(); err != nil {
		t.Logf("plutil -lint: %v\n%s", err, out)
	} else {
		t.Logf("plutil -lint: %s", out)
	}
	uid := strconv.Itoa(os.Getuid())
	for _, domain := range []string{"gui/" + uid, "user/" + uid} {
		out, err := exec.Command("launchctl", "print", domain+"/"+label).CombinedOutput()
		t.Logf("launchctl print %s/%s: err=%v\n%s", domain, label, err, out)
	}
	if out, err := exec.Command("log", "show", "--last", "3m", "--style", "compact",
		"--predicate", `process == "launchd"`).CombinedOutput(); err == nil {
		kept := []string{}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, label) || strings.Contains(line, "pmux") {
				kept = append(kept, line)
			}
		}
		if len(kept) > 40 {
			kept = kept[len(kept)-40:]
		}
		t.Logf("launchd log excerpts:\n%s", strings.Join(kept, "\n"))
	}
	if out, err := exec.Command("launchctl", "list").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "pmux") {
				t.Logf("launchctl list: %s", line)
			}
		}
	}
}

func containsBytes(body, needle []byte) bool {
	return bytes.Contains(body, needle)
}
