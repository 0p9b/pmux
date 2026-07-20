//go:build windows && platform_e2e

package windowstask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
)

func TestPlatformE2EWindowsScheduledTaskLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	platformAdapter, err := platform.New()
	if err != nil {
		t.Fatalf("construct Windows platform adapter: %v", err)
	}
	root := t.TempDir()
	if err := platformAdapter.SecurePermissions(root, true); err != nil {
		requireWindowsReleasePrerequisite(t, "secure private test root", err)
	}
	runtimeDir := filepath.Join(root, "runtime")
	logDir := filepath.Join(root, "logs")
	for _, directory := range []string{runtimeDir, logDir} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		if err := platformAdapter.SecurePermissions(directory, true); err != nil {
			requireWindowsReleasePrerequisite(t, "secure "+directory, err)
		}
	}

	repositoryRoot := repositoryRootFromCaller(t)
	pmuxBinary := filepath.Join(root, "pmux-service-host.exe")
	buildGoBinary(t, ctx, repositoryRoot, pmuxBinary, "./cmd/pmux")
	fakeCore := filepath.Join(root, "fake-cli-proxy-api.exe")
	fakeSource := filepath.Join(root, "fake-core.go")
	if err := os.WriteFile(fakeSource, []byte(fakeCoreSource), 0o600); err != nil {
		t.Fatalf("write fake core source: %v", err)
	}
	buildGoBinary(t, ctx, root, fakeCore, fakeSource)

	port := reserveLoopbackPort(t)
	configPath := filepath.Join(root, "config.yaml")
	configBytes, err := json.Marshal(struct {
		Port       int    `json:"port"`
		RuntimeDir string `json:"runtime_dir"`
	}{Port: port, RuntimeDir: runtimeDir})
	if err != nil {
		t.Fatalf("encode fake core config: %v", err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatalf("write fake core config: %v", err)
	}
	for _, path := range []string{pmuxBinary, fakeCore, configPath} {
		if err := platformAdapter.SecurePermissions(path, false); err != nil {
			requireWindowsReleasePrerequisite(t, "secure "+path, err)
		}
		if err := platformAdapter.VerifySecurePermissions(path, false); err != nil {
			requireWindowsReleasePrerequisite(t, "verify "+path, err)
		}
	}
	for _, directory := range []string{runtimeDir, logDir} {
		if err := platformAdapter.VerifySecurePermissions(directory, true); err != nil {
			requireWindowsReleasePrerequisite(t, "verify "+directory, err)
		}
	}

	instanceID := "platform-e2e-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	spec := service.ServiceSpec{
		InstanceID: instanceID,
		Identity:   service.Identity(service.BackendWindowsTask, instanceID),
		PMuxPath:   pmuxBinary,
		BinaryPath: fakeCore,
		ConfigPath: configPath,
		RuntimeDir: runtimeDir,
		LogDir:     logDir,
	}
	com := NewNativeCOM()
	checker := health.NewPoller(health.HTTPProbe{BaseURL: "http://127.0.0.1:" + strconv.Itoa(port)})
	manager, err := New(spec, com, platformAdapter, NewNativeLogReader(), checker)
	if err != nil {
		t.Fatalf("construct Windows Scheduled Task manager: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = manager.Stop(cleanupCtx, 10*time.Second)
		_ = manager.Uninstall(cleanupCtx)
	}()

	if err := manager.Install(ctx, spec); err != nil {
		requireWindowsReleasePrerequisite(t, "register current-user Task Scheduler task", err)
	}

	registered, err := com.GetTask(ctx, spec.Identity)
	if err != nil {
		t.Fatalf("read registered task through COM: %v", err)
	}
	if registered.Definition.Name != spec.Identity || registered.Definition.Description != OwnershipMarker(instanceID) {
		t.Fatalf("registered identity/ownership = %#v", registered.Definition)
	}
	if registered.Definition.Exec.Executable != pmuxBinary || registered.Definition.Exec.WorkingDirectory != runtimeDir {
		t.Fatalf("registered ExecAction path/working directory = %#v", registered.Definition.Exec)
	}
	wantArguments := []string{
		"--binary", fakeCore,
		"--config", configPath,
		"--runtime-dir", runtimeDir,
		"--log-dir", logDir,
	}
	if strings.Join(registered.Definition.Exec.Arguments, "\x00") != strings.Join(wantArguments, "\x00") {
		t.Fatalf("registered ExecAction arguments = %#v, want %#v", registered.Definition.Exec.Arguments, wantArguments)
	}
	absolutePaths := []string{registered.Definition.Exec.Executable, registered.Definition.Exec.WorkingDirectory}
	for index := 1; index < len(registered.Definition.Exec.Arguments); index += 2 {
		absolutePaths = append(absolutePaths, registered.Definition.Exec.Arguments[index])
	}
	for _, path := range absolutePaths {
		if !filepath.IsAbs(path) {
			t.Fatalf("registered task contains non-absolute path %q", path)
		}
	}
	if !registered.Definition.RunOnlyWhenUserLoggedOn || registered.Definition.RunLevel != RunLevelLeastPrivilege {
		t.Fatalf("task is not current-user least-privilege: %#v", registered.Definition)
	}

	status, err := manager.Status(ctx)
	if err != nil || status.State != service.ServiceStopped {
		t.Fatalf("status after install = %#v, err %v", status, err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start scheduled task and pass health gate: %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != "platform-e2e" {
		t.Fatalf("status after start = %#v, err %v", status, err)
	}

	logReader, err := manager.Logs(ctx, 20, false)
	if err != nil {
		t.Fatalf("open PMux-managed task logs: %v", err)
	}
	logBytes, readErr := io.ReadAll(logReader)
	closeErr := logReader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close PMux logs: read=%v close=%v", readErr, closeErr)
	}
	if !strings.Contains(string(logBytes), "fake core ready") {
		t.Fatalf("PMux-managed logs do not contain fake core startup evidence: %q", logBytes)
	}
	if err := platformAdapter.VerifySecurePermissions(filepath.Join(logDir, "proxy.log"), false); err != nil {
		t.Fatalf("PMux-managed log DACL is not private: %v", err)
	}

	status, err = manager.Restart(ctx)
	if err != nil || status.State != service.ServiceRunning || !status.Healthy {
		t.Fatalf("restart status = %#v, err %v", status, err)
	}
	if err := manager.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop scheduled task: %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceStopped {
		t.Fatalf("status after stop = %#v, err %v", status, err)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Fatalf("uninstall scheduled task: %v", err)
	}
	status, err = manager.Status(ctx)
	if err != nil || status.State != service.ServiceNotInstalled {
		t.Fatalf("status after uninstall = %#v, err %v", status, err)
	}
	if _, err := com.GetTask(ctx, spec.Identity); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("task remains after uninstall: %v", err)
	}
}

func requireWindowsReleasePrerequisite(t *testing.T, operation string, err error) {
	t.Helper()
	if os.Getenv("PMUX_RELEASE_E2E") == "1" {
		t.Fatalf("release prerequisite %s failed: %v", operation, err)
	}
	t.Skipf("Windows platform E2E prerequisite unavailable (%s): %v", operation, err)
}

func repositoryRootFromCaller(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate repository source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", ".."))
}

func buildGoBinary(t *testing.T, ctx context.Context, workingDirectory, output string, packageOrFile ...string) {
	t.Helper()
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	if _, err := os.Stat(goBinary); err != nil {
		var lookErr error
		goBinary, lookErr = exec.LookPath("go.exe")
		if lookErr != nil {
			t.Fatalf("locate Go toolchain for platform E2E helper: %v", lookErr)
		}
	}
	arguments := append([]string{"build", "-trimpath", "-o", output}, packageOrFile...)
	command := exec.CommandContext(ctx, goBinary, arguments...)
	command.Dir = workingDirectory
	outputBytes, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build Windows E2E executable: %v\n%s", err, outputBytes)
	}
}

func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved loopback port: %v", err)
	}
	return port
}

const fakeCoreSource = `package main

import (
    "encoding/json"
    "flag"
    "fmt"
    "net/http"
    "os"
)

func main() {
    configPath := flag.String("config", "", "absolute fake config path")
    flag.Parse()
    body, err := os.ReadFile(*configPath)
    if err != nil { panic(err) }
    var config struct { Port int ` + "`json:\"port\"`" + `; RuntimeDir string ` + "`json:\"runtime_dir\"`" + ` }
    if err := json.Unmarshal(body, &config); err != nil { panic(err) }
    cwd, err := os.Getwd()
    if err != nil { panic(err) }
    if cwd != config.RuntimeDir { panic(fmt.Sprintf("working directory = %q, want %q", cwd, config.RuntimeDir)) }
    http.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
        writer.Header().Set("X-CPA-VERSION", "platform-e2e")
        writer.WriteHeader(http.StatusOK)
        _, _ = writer.Write([]byte("ok"))
    })
    fmt.Println("fake core ready")
    if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", config.Port), nil); err != nil { panic(err) }
}
`
