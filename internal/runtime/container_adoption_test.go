package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/adapter/discovery"
	adapterdoctor "github.com/0p9b/pmux/internal/adapter/doctor"
	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/app"
	domainclient "github.com/0p9b/pmux/internal/domain/client"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

type containerEnumeratorStub struct{ values []discovery.ContainerEvidence }

func (s containerEnumeratorStub) Containers(context.Context) ([]discovery.ContainerEvidence, error) {
	return append([]discovery.ContainerEvidence(nil), s.values...), nil
}

type listenerProbeStub struct{ evidence discovery.PortEvidence }

func (s listenerProbeStub) Probe(context.Context, string) (discovery.PortEvidence, error) {
	return s.evidence, nil
}

type processEnumeratorStub struct{}

func (processEnumeratorStub) Processes(context.Context) ([]discovery.ProcessEvidence, error) {
	return nil, nil
}

type serviceEnumeratorStub struct{}

func (serviceEnumeratorStub) Services(context.Context) ([]discovery.ServiceEvidence, error) {
	return nil, nil
}
func TestContainerAdoptionIsReadOnlyAndEveryMutationPathIsRefused(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	configBody := []byte("host: 127.0.0.1\nport: 8317\nauth-dir: /container/auth\napi-keys:\n  - sk-container-test-key-not-real\nremote-management:\n  allow-remote: false\n  disable-control-panel: true\nws-auth: true\n")
	if err := os.WriteFile(configPath, configBody, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			response.Header().Set("X-CPA-VERSION", "7.2.92")
			response.WriteHeader(http.StatusOK)
		case "/v1/models":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"object":"list","data":[]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	endpoint := strings.TrimPrefix(server.URL, "http://")

	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-root"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache-root"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data-root"))
	platform, err := adapterplatform.New(filepath.Join(root, "config-root"))
	if err != nil {
		t.Fatal(err)
	}
	roots, err := loadRoots(platform)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(state.Paths{
		Config:  filepath.Join(roots.Config, "config.json"),
		State:   filepath.Join(roots.State, "state.json"),
		Secrets: filepath.Join(roots.State, "secrets.json"),
	})
	if err != nil {
		t.Fatal(err)
	}

	container := discovery.ContainerEvidence{
		Runtime: "docker", ID: "0123456789ab", Name: "cpa",
		Image: "eceasy/cli-proxy-api:7.2.92", ConfigMount: configPath,
		PublishedPorts: []discovery.PublishedPortEvidence{{HostIP: "127.0.0.1", HostPort: server.Listener.Addr().(*net.TCPAddr).Port, ContainerPort: 8317, Protocol: "tcp"}},
	}
	mutationFactoryCalls := 0
	native := &nativeRuntime{
		platform: platform, roots: roots, store: store,
		discover: func() discovery.Discoverer {
			return discovery.Discoverer{
				Processes: processEnumeratorStub{}, Services: serviceEnumeratorStub{},
				Containers: containerEnumeratorStub{values: []discovery.ContainerEvidence{container}},
				Listeners:  listenerProbeStub{evidence: discovery.PortEvidence{Address: endpoint, Healthy: true, CoreVersion: "7.2.92"}},
			}
		},
		serviceFactory: func(context.Context, state.Installation, bool) (service.ServiceManager, error) {
			mutationFactoryCalls++
			return nil, errors.New("native lifecycle factory must not receive container installations")
		},
	}

	out, err := native.adopt(context.Background(), app.SetupRequest{Mode: "adopt"})
	if err != nil {
		t.Fatal(err)
	}
	installation := out.Installation
	if installation.Kind != "container" || installation.ServiceBackend != string(service.BackendDockerUnmanaged) || installation.Container == nil {
		t.Fatalf("container adoption was not persisted accurately: %#v", installation)
	}
	if installation.Container.Runtime != "docker" || installation.Container.ID != container.ID || installation.Container.Image != container.Image || installation.Container.Endpoint != endpoint || installation.Container.ConfigMount != configPath {
		t.Fatalf("container evidence was lost: %#v", installation.Container)
	}
	persisted, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Installations) != 1 || persisted.Installations[0].Container == nil {
		t.Fatalf("container installation was not durably recorded: %#v", persisted.Installations)
	}
	afterBody, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBody, configBody) || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatal("read-only container adoption changed the source config")
	}

	manager, err := native.service(context.Background(), installation, false)
	if err != nil {
		t.Fatal(err)
	}
	if mutationFactoryCalls != 0 {
		t.Fatalf("container service routing invoked a native mutation factory %d time(s)", mutationFactoryCalls)
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Backend != service.BackendDockerUnmanaged || status.State != service.ServiceRunning || !status.Healthy || status.CoreVersion != "7.2.92" {
		t.Fatalf("unexpected read-only container status: %#v", status)
	}
	assertContainerMutationDenied(t, manager.Start(context.Background()))
	assertContainerMutationDenied(t, manager.Stop(context.Background(), 0))
	_, restartErr := manager.Restart(context.Background())
	assertContainerMutationDenied(t, restartErr)
	assertContainerMutationDenied(t, manager.Install(context.Background(), service.ServiceSpec{}))
	assertContainerMutationDenied(t, manager.Uninstall(context.Background()))
	if _, err := manager.Logs(context.Background(), 10, false); err == nil {
		t.Fatal("container runtime logs unexpectedly became a lifecycle-managed stream")
	}

	configuration, err := native.config(context.Background(), installation)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := configuration.Read(context.Background(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Path != configPath {
		t.Fatalf("read-only config inspection returned %q", snapshot.Path)
	}
	if _, err := configuration.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{Path: "port", Value: 9000}}); err == nil {
		t.Fatal("container config mutation unexpectedly produced a plan")
	} else {
		assertContainerMutationDenied(t, err)
	}

	authenticator, err := native.auth(context.Background(), installation, management.ProviderID("codex"))
	if err != nil {
		assertContainerMutationDenied(t, err)
	} else if _, err := authenticator.Begin(context.Background(), provider.FlowDeviceCode); err == nil {
		t.Fatal("container authentication unexpectedly started")
	} else {
		assertContainerMutationDenied(t, err)
	}
	launcher, err := native.launcher(context.Background(), installation, app.Invocation{})
	if err != nil {
		assertContainerMutationDenied(t, err)
	} else if _, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{}); err == nil {
		t.Fatal("container client launch unexpectedly started")
	} else {
		assertContainerMutationDenied(t, err)
	}
	if _, err := native.Proxy(context.Background(), installation, "7.2.93"); err == nil {
		t.Fatal("container proxy update unexpectedly started")
	} else {
		assertContainerMutationDenied(t, err)
	}
	if _, err := native.Restore(context.Background(), configPath, "unused"); err == nil {
		t.Fatal("container config restore unexpectedly started")
	} else {
		assertContainerMutationDenied(t, err)
	}
	if _, _, err := native.Run(context.Background(), installation, nil, nil, true, true, false); err == nil {
		t.Fatal("container doctor fix unexpectedly started")
	} else {
		assertContainerMutationDenied(t, err)
	}

	report, unhealthy, err := native.Run(context.Background(), installation, nil, nil, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if unhealthy {
		var failed strings.Builder
		for _, check := range report.(adapterdoctor.Report).Checks {
			if check.Status == "fail" {
				failed.WriteString(check.ID + ":" + check.Summary + ";")
			}
		}
		t.Fatalf("read-only container doctor unexpectedly reported unresolved failures: %s", failed.String())
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.ToLower(string(reportJSON))
	for _, required := range []string{"docker", container.ID, strings.ToLower(container.Image), strings.ToLower(endpoint), "external container"} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("doctor output omitted container evidence %q: %s", required, rendered)
		}
	}
	for _, forbidden := range []string{"binary is missing", "configuration file is not readable"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("doctor treated a container-only invariant as native failure %q: %s", forbidden, rendered)
		}
	}
	if mutationFactoryCalls != 0 {
		t.Fatalf("read-only doctor invoked the native service factory %d time(s)", mutationFactoryCalls)
	}
	finalBody, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(finalBody, configBody) {
		t.Fatal("container status or doctor changed the source config")
	}
}

func assertContainerMutationDenied(t *testing.T, values ...any) {
	t.Helper()
	var err error
	for _, value := range values {
		if candidate, ok := value.(error); ok && candidate != nil {
			err = candidate
		}
	}
	if err == nil {
		t.Fatal("container mutation unexpectedly succeeded")
	}
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.ServiceForeignOwner {
		t.Fatalf("container mutation returned %T %v, want %s", err, err, pmuxerr.ServiceForeignOwner)
	}
	if !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("container mutation omitted owning-runtime guidance: %v", err)
	}
}
