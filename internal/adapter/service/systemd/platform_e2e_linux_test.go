//go:build linux && platform_e2e

package systemd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/foreground"
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
)

const platformE2ESecret = "sk-platform-e2e-secret-1234567890"

type platformFixture struct {
	root, pmux, core, config, runtimeDir, logDir, evidence, baseURL string
}

type coreEvidence struct {
	ConfigPath string `json:"config_path"`
	PGStore    string `json:"pgstore"`
}

func TestPlatformE2EForegroundAttachedLifecycle(t *testing.T) {
	fixture := buildPlatformFixture(t, false)
	checker := health.NewPoller(health.HTTPProbe{BaseURL: fixture.baseURL})
	pidPath := filepath.Join(fixture.root, "foreground.pid")
	spec := fixture.spec(service.BackendForeground, "platform-foreground")

	first := foreground.NewAttachedPersistent(foreground.OSRunner{}, checker, pidPath, foreground.Streams{
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
	})
	if err := first.Install(context.Background(), spec); err != nil {
		t.Fatalf("install foreground: %v", err)
	}
	wait, err := first.StartAttached(context.Background())
	if err != nil {
		t.Fatalf("start attached foreground: %v", err)
	}
	status, err := first.Status(context.Background())
	if err != nil || status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != "platform-e2e" {
		t.Fatalf("foreground running status=%#v err=%v", status, err)
	}
	assertCoreEvidence(t, fixture)

	// A second manager models `pmux service stop` from another invocation. It
	// must validate the durable PID/start evidence before signaling the group.
	second := foreground.NewPersistent(foreground.OSRunner{}, checker, pidPath)
	if err := second.Install(context.Background(), spec); err != nil {
		t.Fatalf("recover foreground ownership: %v", err)
	}
	recovered, err := second.Status(context.Background())
	if err != nil || recovered.State != service.ServiceRunning || recovered.PID <= 0 {
		t.Fatalf("recovered status=%#v err=%v", recovered, err)
	}
	if err := second.Stop(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("cross-invocation stop: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wait(waitCtx); err != nil {
		t.Fatalf("attached waiter: %v", err)
	}
	if err := second.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstall foreground: %v", err)
	}
	status, err = second.Status(context.Background())
	if err != nil || status.State != service.ServiceNotInstalled {
		t.Fatalf("foreground uninstalled status=%#v err=%v", status, err)
	}
}
func TestPlatformE2ESystemdUserLifecycle(t *testing.T) {
	if output, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		message := fmt.Sprintf("systemd user bus unavailable: %v: %s", err, strings.TrimSpace(string(output)))
		if os.Getenv("PMUX_RELEASE_E2E") == "1" {
			t.Fatal(message)
		}
		t.Skip(message)
	}

	fixture := buildPlatformFixture(t, true)
	instanceID := fmt.Sprintf("platform-e2e-%d", os.Getpid())
	unitDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	unitDir = filepath.Join(unitDir, "systemd", "user")
	checker := health.NewPoller(health.HTTPProbe{BaseURL: fixture.baseURL})
	manager := New(instanceID, unitDir, OSRunner{}, checker)
	spec := fixture.spec(service.BackendSystemdUser, instanceID)
	defer func() { _ = manager.Uninstall(context.Background()) }()

	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatalf("systemd install: %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		output, _ := exec.Command("systemctl", "--user", "status", spec.Identity, "--no-pager").CombinedOutput()
		loadError, _ := exec.Command("systemctl", "--user", "show", spec.Identity, "-p", "LoadError", "-p", "FragmentPath").CombinedOutput()
		unitBody, _ := os.ReadFile(filepath.Join(unitDir, spec.Identity))
		t.Fatalf("systemd start: %#v\n%s\n%s\n%s", err, output, loadError, unitBody)
	}
	status, err := manager.Status(context.Background())
	if err != nil || status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != "platform-e2e" {
		t.Fatalf("systemd running status=%#v err=%v", status, err)
	}
	assertCoreEvidence(t, fixture)
	assertSystemdLogs(t, manager)

	status, err = manager.Restart(context.Background())
	if err != nil || status.State != service.ServiceRunning || !status.Healthy {
		t.Fatalf("systemd restart status=%#v err=%v", status, err)
	}
	if err := manager.Stop(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("systemd stop: %v", err)
	}
	status, err = manager.Status(context.Background())
	if err != nil || status.State != service.ServiceStopped {
		t.Fatalf("systemd stopped status=%#v err=%v", status, err)
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("systemd uninstall: %v", err)
	}
	status, err = manager.Status(context.Background())
	if err != nil || status.State != service.ServiceNotInstalled {
		t.Fatalf("systemd uninstalled status=%#v err=%v", status, err)
	}
}

func buildPlatformFixture(t *testing.T, buildPMux bool) platformFixture {
	t.Helper()
	root := t.TempDir()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate repository root")
	}
	repository := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "..", ".."))

	coreSource := filepath.Join(root, "fake-core.go")
	if err := os.WriteFile(coreSource, []byte(platformCoreSource), 0o600); err != nil {
		t.Fatal(err)
	}
	corePath := filepath.Join(root, "fake-core")
	buildBinary(t, repository, corePath, coreSource)
	body, err := os.ReadFile(corePath)
	if err != nil {
		t.Fatal(err)
	}
	pmuxPath := corePath
	if buildPMux {
		hostSource := filepath.Join(root, "fake-service-host.go")
		if err := os.WriteFile(hostSource, []byte(platformServiceHostSource), 0o600); err != nil {
			t.Fatal(err)
		}
		pmuxPath = filepath.Join(root, "fake-service-host")
		buildBinary(t, repository, pmuxPath, hostSource)
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) == strings.Repeat("0", 64) {
		t.Fatal("fake core checksum was not computed")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	runtimeDir := filepath.Join(root, "runtime")
	logDir := filepath.Join(root, "logs")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(root, "evidence.json")
	config := filepath.Join(root, "config.json")
	configBody, _ := json.Marshal(map[string]string{"address": address, "evidence": evidence})
	if err := os.WriteFile(config, configBody, 0o600); err != nil {
		t.Fatal(err)
	}
	return platformFixture{root: root, pmux: pmuxPath, core: corePath, config: config, runtimeDir: runtimeDir, logDir: logDir, evidence: evidence, baseURL: "http://" + address}
}

func buildBinary(t *testing.T, repository, output string, target ...string) {
	t.Helper()
	args := append([]string{"build", "-trimpath", "-o", output}, target...)
	command := exec.Command("go", args...)
	command.Dir = repository
	if body, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, body)
	}
}

func (f platformFixture) spec(backend service.ServiceBackend, instanceID string) service.ServiceSpec {
	return service.ServiceSpec{
		InstanceID: instanceID, Identity: service.Identity(backend, instanceID), PMuxPath: f.pmux,
		BinaryPath: f.core, ConfigPath: f.config, RuntimeDir: f.runtimeDir, LogDir: f.logDir,
		Environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "PGSTORE_URL=must-not-reach-core", "ANTHROPIC_AUTH_TOKEN=must-not-reach-core"},
	}
}

func assertCoreEvidence(t *testing.T, fixture platformFixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, err := os.ReadFile(fixture.evidence)
		if err == nil {
			var evidence coreEvidence
			if json.Unmarshal(body, &evidence) != nil {
				t.Fatalf("invalid core evidence: %s", body)
			}
			if evidence.ConfigPath != fixture.config || evidence.PGStore != "" {
				t.Fatalf("unsafe core evidence: %#v", evidence)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("core evidence unavailable: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertSystemdLogs(t *testing.T, manager *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stream, err := manager.Logs(context.Background(), 100, false)
		if err != nil {
			t.Fatalf("systemd logs: %v", err)
		}
		body, readErr := io.ReadAll(stream)
		_ = stream.Close()
		if readErr != nil {
			t.Fatalf("read systemd logs: %v", readErr)
		}
		if bytes.Contains(body, []byte(platformE2ESecret)) {
			t.Fatalf("systemd logs disclosed secret: %s", body)
		}
		if bytes.Contains(body, []byte("platform fake core ready")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("systemd log marker unavailable: %s", body)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

const platformServiceHostSource = `package main

import (
    "os"
    "path/filepath"
    "syscall"
)

func main() {
    values := map[string]string{}
    for i := 1; i+1 < len(os.Args); i += 2 { values[os.Args[i]] = os.Args[i+1] }
    binary, config, runtimeDir := values["--binary"], values["--config"], values["--runtime-dir"]
    if !filepath.IsAbs(binary) || !filepath.IsAbs(config) || !filepath.IsAbs(runtimeDir) { panic("absolute service-host paths are required") }
    if _, err := os.Stat(filepath.Join(runtimeDir, ".env")); err == nil { panic("runtime .env is forbidden") }
    if err := os.Chdir(runtimeDir); err != nil { panic(err) }
    environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
    if err := syscall.Exec(binary, []string{binary, "-config", config}, environment); err != nil { panic(err) }
}
`

const platformCoreSource = `package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "os"
    "path/filepath"
)

type config struct { Address string ` + "`json:\"address\"`" + `; Evidence string ` + "`json:\"evidence\"`" + ` }
type evidence struct { ConfigPath string ` + "`json:\"config_path\"`" + `; PGStore string ` + "`json:\"pgstore\"`" + ` }
func main() {
    if len(os.Args) != 3 || os.Args[1] != "-config" || !filepath.IsAbs(os.Args[2]) { panic("absolute -config is required") }
    body, err := os.ReadFile(os.Args[2]); if err != nil { panic(err) }
    var cfg config; if err := json.Unmarshal(body, &cfg); err != nil { panic(err) }
    observed, _ := json.Marshal(evidence{ConfigPath: os.Args[2], PGStore: os.Getenv("PGSTORE_URL")})
    if err := os.WriteFile(cfg.Evidence, observed, 0600); err != nil { panic(err) }
    fmt.Fprintln(os.Stderr, "platform fake core ready Authorization: Bearer ` + platformE2ESecret + `")
    http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-CPA-VERSION", "platform-e2e"); w.WriteHeader(http.StatusOK) })
    if err := http.ListenAndServe(cfg.Address, nil); err != nil { panic(err) }
}
`
