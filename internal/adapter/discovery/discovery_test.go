package discovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestDiscoveryIsReadOnlyForFilesAndProcesses(t *testing.T) {
	if os.Getenv("PMUX_DISCOVERY_HELPER") == "1" {
		select {}
	}
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("host: 127.0.0.1\nport: 8317\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(beforeBytes)

	command := exec.Command(os.Args[0], "-test.run=TestDiscoveryIsReadOnlyForFilesAndProcesses")
	command.Args[0] = "cli-proxy-api"
	command.Args = append(command.Args, "--", "-config", config)
	command.Env = append(os.Environ(), "PMUX_DISCOVERY_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _, _ = command.Process.Wait() })

	discoverer := Discoverer{
		Processes: LocalProcessEnumerator{},
		Versions:  VersionDetector{},
		LookPath:  func(string) (string, error) { return "", exec.ErrNotFound },
	}
	candidates, err := discoverer.Discover(context.Background(), Request{ConfigPath: config})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, candidate := range candidates {
		if candidate.Process != nil && candidate.Process.PID == command.Process.Pid {
			found = true
			if candidate.Process.ConfigPath != config {
				t.Fatalf("wrong process config: %q", candidate.Process.ConfigPath)
			}
		}
	}
	if !found {
		t.Fatalf("live CLIProxyAPI-shaped process %d was not discovered: %#v", command.Process.Pid, candidates)
	}

	if _, err := os.Stat(filepath.Join(root, "anything-created-by-discovery")); !os.IsNotExist(err) {
		t.Fatalf("unexpected artifact: %v", err)
	}
	afterInfo, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	afterHash := sha256.Sum256(afterBytes)
	if beforeHash != afterHash || beforeInfo.Mode() != afterInfo.Mode() || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("discovery changed the source config")
	}
	procState, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", command.Process.Pid))
	if err != nil || len(procState) == 0 {
		t.Fatalf("discovery changed or terminated the source process: %v", err)
	}
}

func TestLoopbackPortDiscoveryCapturesRunningVersionHeader(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/healthz" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("X-CPA-VERSION", "7.2.92")
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	discoverer := Discoverer{
		Listeners: HTTPListenerProber{Client: server.Client()},
		Versions:  VersionDetector{},
		LookPath:  func(string) (string, error) { return "", exec.ErrNotFound },
	}
	candidates, err := discoverer.Discover(context.Background(), Request{Addresses: []string{address}})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Port == nil || !candidates[0].Port.Healthy {
		t.Fatalf("loopback listener was not discovered: %#v", candidates)
	}
	if candidates[0].Version.Source != VersionRunningHeader || candidates[0].Version.Version != "7.2.92" {
		t.Fatalf("running header was not used: %#v", candidates[0].Version)
	}
}

type metadataStub struct {
	version VersionEvidence
	ok      bool
	calls   int
}

func (m *metadataStub) Version(context.Context, string) (VersionEvidence, bool, error) {
	m.calls++
	return m.version, m.ok, nil
}

type proberStub struct {
	version ProbeVersion
	err     error
	calls   int
}

func (p *proberStub) Probe(context.Context, string) (ProbeVersion, error) {
	p.calls++
	return p.version, p.err
}

func TestVersionDetectionOrder(t *testing.T) {
	t.Parallel()
	binary := &FileEvidence{Path: "/absolute/cli-proxy-api"}
	metadata := &metadataStub{version: VersionEvidence{Version: "7.2.91"}, ok: true}
	probe := &proberStub{version: ProbeVersion{Version: "7.2.90"}}
	detector := VersionDetector{Metadata: metadata, Probe: probe}

	running := detector.Detect(context.Background(), Candidate{Binary: binary, Port: &PortEvidence{CoreVersion: "7.2.92"}})
	if running.Source != VersionRunningHeader || running.Version != "7.2.92" {
		t.Fatalf("running header did not win: %#v", running)
	}
	if metadata.calls != 0 || probe.calls != 0 {
		t.Fatal("lower-precedence detectors ran after the header succeeded")
	}

	fromMetadata := detector.Detect(context.Background(), Candidate{Binary: binary})
	if fromMetadata.Source != VersionMetadata || fromMetadata.Version != "7.2.91" {
		t.Fatalf("metadata did not win: %#v", fromMetadata)
	}
	if metadata.calls != 1 || probe.calls != 0 {
		t.Fatal("probe ran after metadata succeeded")
	}

	metadata.ok = false
	fromProbe := detector.Detect(context.Background(), Candidate{Binary: binary})
	if fromProbe.Source != VersionIsolatedProbe || fromProbe.Version != "7.2.90" {
		t.Fatalf("probe did not run: %#v", fromProbe)
	}
}

func TestUnsafeVersionProbeRecordsUnknown(t *testing.T) {
	t.Parallel()
	probe := &proberStub{err: ErrUnsafeVersionProbe}
	version := (VersionDetector{Probe: probe}).Detect(context.Background(), Candidate{Binary: &FileEvidence{Path: "/not/safe"}})
	if version.Source != VersionUnknown || version.Version != "unknown" {
		t.Fatalf("unsafe probe was not recorded unknown: %#v", version)
	}
	containerVersion := (VersionDetector{Probe: probe}).Detect(context.Background(), Candidate{Container: &ContainerEvidence{ID: "c"}})
	if containerVersion.Source != VersionUnknown || probe.calls != 1 {
		t.Fatalf("container unexpectedly probed local binary: %#v calls=%d", containerVersion, probe.calls)
	}
}

type containerEnumeratorStub struct{ values []ContainerEvidence }

func (s containerEnumeratorStub) Containers(context.Context) ([]ContainerEvidence, error) {
	return s.values, nil
}

type recordingTransport struct{ methods []string }

func (r *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.methods = append(r.methods, request.Method+" "+request.URL.Path)
	body := `[{"Id":"abc","Names":["/core"],"Image":"eceasy/cli-proxy-api:latest","State":"running","Mounts":[{"Source":"/tmp/config.yaml","Destination":"/CLIProxyAPI/config.yaml","RW":false}],"Ports":[]}]`
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

func TestDockerDiscoveryOnlyUsesReadOnlyInventoryAndLifecycleUnavailable(t *testing.T) {
	t.Parallel()
	transport := &recordingTransport{}
	enumerator := DockerSocketEnumerator{Client: &http.Client{Transport: transport}}
	containers, err := enumerator.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].ID != "abc" {
		t.Fatalf("unexpected containers: %#v", containers)
	}
	if len(transport.methods) != 1 || transport.methods[0] != "GET /v1.41/containers/json" {
		t.Fatalf("Docker discovery made a mutation request: %#v", transport.methods)
	}

	discoverer := Discoverer{Containers: containerEnumeratorStub{values: containers}, LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	candidates, err := discoverer.Discover(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Container == nil || !candidates[0].ToInstallation("docker").Container {
		t.Fatalf("container adoption evidence missing: %#v", candidates)
	}
	if !strings.Contains(strings.Join(candidates[0].Findings, " "), "externally managed") {
		t.Fatalf("container ownership finding missing: %#v", candidates[0].Findings)
	}

	manager := DockerServiceManager{Container: containers[0]}
	mutations := []func() error{
		func() error { return manager.Install(context.Background(), service.ServiceSpec{}) },
		func() error { return manager.Start(context.Background()) },
		func() error { return manager.Stop(context.Background(), time.Second) },
		func() error { _, err := manager.Restart(context.Background()); return err },
		func() error { return manager.Uninstall(context.Background()) },
	}
	for index, mutate := range mutations {
		err := mutate()
		var typed *pmuxerr.Error
		if !errors.As(err, &typed) || typed.Code != pmuxerr.ServiceForeignOwner || pmuxerr.ExitCode(err) != 9 {
			t.Fatalf("mutation %d was not refused as foreign ownership: %v", index, err)
		}
	}
}

func TestServiceDefinitionDiscoveryIsReadOnlyAndFindsAbsoluteConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "cli-proxy-api")
	config := filepath.Join(root, "config.yaml")
	runtimeDir := filepath.Join(root, "runtime")
	for path, body := range map[string]string{binary: "binary", config: "host: 127.0.0.1"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "cliproxyapi.service")
	unitBody := "[Service]\nWorkingDirectory=" + runtimeDir + "\nExecStart=" + binary + " -config " + config + "\n"
	if err := os.WriteFile(unit, []byte(unitBody), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(unit)
	services, err := (LocalServiceEnumerator{Paths: []string{unit}, GOOS: "linux"}).Services(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].ConfigPath != config || services[0].Executable != binary {
		t.Fatalf("unexpected service evidence: %#v", services)
	}
	after, _ := os.ReadFile(unit)
	if string(before) != string(after) {
		t.Fatal("service discovery changed the definition")
	}
}
