//go:build darwin && platform_e2e

package launchd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
)

const releaseE2EEnvironment = "PMUX_RELEASE_E2E"

type e2eCoreConfig struct {
	Address string `json:"address"`
}

// TestMain also supplies the two real executable boundaries exercised by the
// LaunchAgent: the PMux service host and the fake CLIProxyAPI core. It handles
// them before the testing package parses flags, so launchd invokes ordinary
// executable/argument vectors rather than a shell fixture.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--binary":
			if len(os.Args) != 5 || os.Args[3] != "--config" {
				_, _ = fmt.Fprintln(os.Stderr, "invalid fake service-host arguments")
				os.Exit(2)
			}
			if err := syscall.Exec(os.Args[2], []string{os.Args[2], "--fake-core", "--config", os.Args[4]}, os.Environ()); err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		case "--fake-core":
			if len(os.Args) != 4 || os.Args[2] != "--config" {
				_, _ = fmt.Fprintln(os.Stderr, "invalid fake core arguments")
				os.Exit(2)
			}
			if err := runE2ECore(os.Args[3]); err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestLaunchAgentReleaseE2E(t *testing.T) {
	if os.Getenv(releaseE2EEnvironment) != "1" {
		t.Skip("set PMUX_RELEASE_E2E=1 on a macOS release runner to exercise launchctl")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root := t.TempDir()
	instanceID := "release-e2e-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	label := service.Identity(service.BackendLaunchd, instanceID)
	address := reserveAddress(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	hostPath := filepath.Join(root, "pmux-service-host")
	corePath := filepath.Join(root, "fake-core")
	copyExecutable(t, self, hostPath)
	copyExecutable(t, self, corePath)
	adHocSign(t, hostPath)
	adHocSign(t, corePath)

	configPath := filepath.Join(root, "instance", "config.json")
	writeCoreConfig(t, configPath, e2eCoreConfig{Address: address})

	runtimeDir := filepath.Join(root, "instance", "runtime")
	logDir := filepath.Join(root, "state", "logs")
	home := mustHome(t)
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

func runE2ECore(configPath string) error {
	body, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg e2eCoreConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-CPA-VERSION", "e2e-core")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(stopped)
	go func() {
		<-stopped
		_ = server.Close()
	}()
	_, _ = fmt.Fprintln(os.Stdout, "fake core started")
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
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
	wantArgs := []any{spec.PMuxPath, "--binary", spec.BinaryPath, "--config", spec.ConfigPath}
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

func copyExecutable(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func adHocSign(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("codesign", "-s", "-", "-f", path).CombinedOutput()
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

func containsBytes(body, needle []byte) bool {
	return bytes.Contains(body, needle)
}
