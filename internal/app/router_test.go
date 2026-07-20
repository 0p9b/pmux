package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	domainservice "github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

type memoryStore struct {
	config        state.Config
	state         state.State
	secrets       state.SecretReferences
	saveConfigErr error
}

func (s *memoryStore) LoadConfig() (state.Config, error) {
	if s.config.Version == 0 {
		s.config.Version = state.SchemaVersion
	}
	return s.config, nil
}
func (s *memoryStore) SaveConfig(value state.Config) error {
	if s.saveConfigErr != nil {
		return s.saveConfigErr
	}
	s.config = value
	return nil
}
func (s *memoryStore) LoadState() (state.State, error) {
	if s.state.Version == 0 {
		s.state.Version = state.SchemaVersion
	}
	return s.state, nil
}
func (s *memoryStore) SaveState(value state.State) error                     { s.state = value; return nil }
func (s *memoryStore) LoadSecretReferences() (state.SecretReferences, error) { return s.secrets, nil }

func testRoots() domainplatform.Roots {
	return domainplatform.Roots{Config: "/tmp/config", State: "/tmp/state", Cache: "/tmp/cache", Data: "/tmp/data"}
}

func TestRouterConstructorReturnsTypedError(t *testing.T) {
	_, err := NewRouter(Dependencies{Roots: testRoots()})
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeDependencyMissing {
		t.Fatalf("error = %#v", err)
	}
}

func TestRouterEmptyHomeReadOnlyOperations(t *testing.T) {
	router, err := NewRouter(Dependencies{Roots: testRoots(), Store: &memoryStore{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, invocation := range []Invocation{
		{Operation: OpDashboardStatus, JSON: true},
		{Operation: OpDoctor},
		{Operation: OpConfigShow, Options: map[string]any{"scope": "proxy"}},
		{Operation: OpServiceStatus},
		{Operation: OpModelsList},
	} {
		result, err := router.Execute(context.Background(), invocation, nil)
		if err != nil {
			t.Fatalf("%s: %v", invocation.Operation, err)
		}
		if result.Data == nil {
			t.Fatalf("%s returned nil data", invocation.Operation)
		}
	}
}

type fakeSetup struct{ called bool }

func (f *fakeSetup) Setup(_ context.Context, request SetupRequest) (SetupOutcome, error) {
	f.called = true
	return SetupOutcome{Installation: state.Installation{ID: "default", Kind: request.Mode}, CoreComplete: true}, nil
}

type fakeCatalog struct {
	entries           []domainmodel.CatalogEntry
	listed, refreshed bool
}

func (f *fakeCatalog) List(context.Context) ([]domainmodel.CatalogEntry, error) {
	f.listed = true
	return append([]domainmodel.CatalogEntry(nil), f.entries...), nil
}
func (f *fakeCatalog) Refresh(context.Context) ([]domainmodel.CatalogEntry, error) {
	f.refreshed = true
	return append([]domainmodel.CatalogEntry(nil), f.entries...), nil
}
func (f *fakeCatalog) Attribution(context.Context) (map[string][]management.ProviderID, error) {
	return nil, nil
}

type fakeService struct {
	status     domainservice.ServiceStatus
	started    bool
	restarts   int
	stops      int
	uninstalls int
	logText    string
	logTail    int
	logFollow  bool
}

func (f *fakeService) Backend() domainservice.ServiceBackend { return f.status.Backend }
func (f *fakeService) Detect(context.Context) (domainservice.ServiceStatus, error) {
	return f.status, nil
}
func (f *fakeService) Install(context.Context, domainservice.ServiceSpec) error { return nil }
func (f *fakeService) Uninstall(context.Context) error                          { f.uninstalls++; return nil }
func (f *fakeService) Start(context.Context) error                              { f.started = true; return nil }
func (f *fakeService) Stop(context.Context, time.Duration) error {
	f.stops++
	return nil
}
func (f *fakeService) Restart(context.Context) (domainservice.ServiceStatus, error) {
	f.restarts++
	return f.status, nil
}
func (f *fakeService) Status(context.Context) (domainservice.ServiceStatus, error) {
	return f.status, nil
}
func (f *fakeService) Logs(_ context.Context, tail int, follow bool) (io.ReadCloser, error) {
	f.logTail, f.logFollow = tail, follow
	text := f.logText
	if text == "" {
		text = "safe log\n"
	}
	return io.NopCloser(strings.NewReader(text)), nil
}

type fakeLauncher struct {
	launched     domainclient.LaunchSpec
	launchResult domainclient.LaunchResult
	launchErr    error
	persistPlan  domainclient.PersistPlan
	upserts      []domainclient.PersistPlan
}

func (f *fakeLauncher) Client() domainclient.ClientID { return domainclient.Claude }
func (f *fakeLauncher) Detect(context.Context) (domainclient.ClientInstall, error) {
	return domainclient.ClientInstall{Path: "/bin/claude", Version: "2.1.0", Supported: true}, nil
}
func (f *fakeLauncher) Env(domainclient.LaunchSpec) ([]string, error) { return nil, nil }
func (f *fakeLauncher) Launch(_ context.Context, spec domainclient.LaunchSpec) (domainclient.LaunchResult, error) {
	f.launched = spec
	return f.launchResult, f.launchErr
}
func (f *fakeLauncher) PlanPersist(context.Context, domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	return f.persistPlan, nil
}
func (f *fakeLauncher) Upsert(_ context.Context, plan domainclient.PersistPlan) error {
	f.upserts = append(f.upserts, plan)
	return nil
}
func (f *fakeLauncher) Unpersist(context.Context) error { return nil }

type fakeConfig struct{ snapshot domainconfig.ConfigSnapshot }

func (f fakeConfig) Read(context.Context, string) (domainconfig.ConfigSnapshot, error) {
	return f.snapshot, nil
}
func (f fakeConfig) Plan(context.Context, domainconfig.ConfigSnapshot, []domainconfig.PatchOp) (domainconfig.PatchPlan, error) {
	return domainconfig.PatchPlan{}, nil
}
func (f fakeConfig) Apply(context.Context, domainconfig.PatchPlan) (domainconfig.PatchResult, error) {
	return domainconfig.PatchResult{}, nil
}
func (f fakeConfig) Validate(context.Context, domainconfig.ConfigSnapshot) []domainconfig.Diagnostic {
	return nil
}

func TestRouterFixtureRoutesAdaptersAndKeepsSecretInternal(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed", BinaryPath: "/tmp/core", ConfigPath: "/tmp/config.yaml", ProxyKeyRef: state.SecretReference{Path: "/tmp/api-key.txt", Masked: "sk-abcd…wxyz", Fingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, AuthDir: "/tmp/auth", RuntimeDir: "/tmp/runtime", Host: "127.0.0.1", Port: 8317, ServiceBackend: "foreground"}
	store := &memoryStore{state: state.State{Version: state.SchemaVersion, Installations: []state.Installation{installation}}}
	catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{ID: "live-model", Available: true, Source: "cache"}}}
	serviceManager := &fakeService{status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground, State: domainservice.ServiceRunning, Healthy: true, CoreVersion: "7.2.92"}}
	launcher := &fakeLauncher{}
	setup := &fakeSetup{}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store, Setup: setup,
		Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
		Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
			return serviceManager, nil
		},
		Configs: func(context.Context, state.Installation) (domainconfig.ConfigFile, error) {
			return fakeConfig{snapshot: domainconfig.ConfigSnapshot{Path: installation.ConfigPath, Config: domainconfig.Config{Host: "127.0.0.1", Port: 8317, APIKeys: []string{"full-secret-must-not-render"}}}}, nil
		},
		Launcher: func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error) {
			return launcher, nil
		},
		Secrets: func(context.Context, state.Installation) ([]byte, error) {
			return []byte("full-secret-must-remain-internal"), nil
		},
		WorkingDir: func() (string, error) { return "/tmp/project", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := router.Execute(context.Background(), Invocation{Operation: OpSetup, Options: map[string]any{"mode": "managed"}, Yes: true}, nil); err != nil || !setup.called {
		t.Fatalf("setup called=%v err=%v", setup.called, err)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpServiceStatus}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpModelsList}, nil); err != nil || !catalog.listed {
		t.Fatalf("models listed=%v err=%v", catalog.listed, err)
	}
	configResult, err := router.Execute(context.Background(), Invocation{Operation: OpConfigShow, Options: map[string]any{"scope": "proxy"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(toJSON(configResult.Data))), "full-secret") {
		t.Fatal("config result disclosed a complete secret")
	}
	preflight, err := router.Execute(context.Background(), Invocation{Operation: OpLaunchPreflight, Options: map[string]any{"client": "claude", "model": "live-model"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.launched.Model != "" {
		t.Fatal("launch preflight started the client")
	}
	if body := toJSON(preflight.Data); strings.Contains(body, "full-secret") || !strings.Contains(body, `"version":"2.1.0"`) {
		t.Fatalf("unsafe or incomplete preflight result: %s", body)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpLaunch, Options: map[string]any{"client": "claude", "model": "live-model"}}, nil); err != nil {
		t.Fatal(err)
	}
	if launcher.launched.Token != "full-secret-must-remain-internal" || launcher.launched.Model != "live-model" {
		t.Fatalf("launch spec = %#v", launcher.launched)
	}
}

func TestRouterReturnsClientExitStatusWithoutPMuxError(t *testing.T) {
	for _, test := range []struct {
		name         string
		clientResult domainclient.LaunchResult
		json         bool
		wantHuman    string
	}{
		{name: "ordinary", clientResult: domainclient.LaunchResult{ExitCode: 42}, json: true, wantHuman: "status 42"},
		{name: "signal", clientResult: domainclient.LaunchResult{ExitCode: 143, Signal: "terminated"}, wantHuman: "status 143"},
	} {
		t.Run(test.name, func(t *testing.T) {
			installation := state.Installation{ID: "default", Kind: "managed", ProxyKeyRef: state.SecretReference{Path: "/tmp/key"}, Host: "127.0.0.1", Port: 8317}
			store := configuredState(installation)
			catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{ID: "live-model", Available: true}}}
			launcher := &fakeLauncher{launchResult: test.clientResult}
			var factoryInvocation Invocation
			router, err := NewRouter(Dependencies{
				Roots: testRoots(), Store: store,
				Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
				Launcher: func(_ context.Context, _ state.Installation, in Invocation) (domainclient.ClientLauncher, error) {
					factoryInvocation = in
					return launcher, nil
				},
				Secrets:    func(context.Context, state.Installation) ([]byte, error) { return []byte("sk-private"), nil },
				WorkingDir: func() (string, error) { return "/tmp", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := router.Execute(context.Background(), Invocation{
				Operation: OpLaunch, JSON: test.json,
				Options: map[string]any{"client": "claude", "model": "live-model"},
			}, nil)
			if err != nil {
				t.Fatalf("launch returned PMux error for client exit: %v", err)
			}
			if result.ExitCode != test.clientResult.ExitCode {
				t.Fatalf("ExitCode = %d, want %d", result.ExitCode, test.clientResult.ExitCode)
			}
			data, ok := result.Data.(map[string]any)
			if !ok || data["origin"] != "client" || data["exit_code"] != test.clientResult.ExitCode {
				t.Fatalf("launch result data = %#v", result.Data)
			}
			if test.clientResult.Signal != "" && data["signal"] != test.clientResult.Signal {
				t.Fatalf("signal data = %#v", data["signal"])
			}
			if len(result.Human) != 1 || !strings.Contains(result.Human[0], test.wantHuman) {
				t.Fatalf("human = %#v", result.Human)
			}
			if factoryInvocation.JSON != test.json {
				t.Fatalf("launcher factory JSON = %v, want %v", factoryInvocation.JSON, test.json)
			}
		})
	}
}

func toJSON(value any) string { body, _ := json.Marshal(value); return string(body) }

type logManagement struct {
	management.ManagementClient
	deleted      bool
	settings     map[management.SettingName]management.SettingValue
	putHistory   []management.SettingValue
	mismatchOnce bool
}

func (f *logManagement) DeleteLogs(context.Context) error {
	f.deleted = true
	return nil
}

func (f *logManagement) GetSetting(_ context.Context, name management.SettingName) (management.SettingValue, error) {
	if f.settings == nil {
		f.settings = make(map[management.SettingName]management.SettingValue)
	}
	value := f.settings[name]
	if len(value) == 0 {
		value = management.SettingValue(`{"value":false}`)
	}
	return append(management.SettingValue(nil), value...), nil
}

func (f *logManagement) PutSetting(_ context.Context, name management.SettingName, value management.SettingValue) error {
	if f.settings == nil {
		f.settings = make(map[management.SettingName]management.SettingValue)
	}
	f.putHistory = append(f.putHistory, append(management.SettingValue(nil), value...))
	if f.mismatchOnce {
		f.mismatchOnce = false
		f.settings[name] = management.SettingValue(`{"value":999}`)
		return nil
	}
	f.settings[name] = append(management.SettingValue(nil), value...)
	return nil
}

type classifyingConfig struct {
	snapshot domainconfig.ConfigSnapshot
	planned  domainconfig.PatchPlan
}

func (f *classifyingConfig) Read(context.Context, string) (domainconfig.ConfigSnapshot, error) {
	return f.snapshot, nil
}
func (f *classifyingConfig) Plan(_ context.Context, snapshot domainconfig.ConfigSnapshot, ops []domainconfig.PatchOp) (domainconfig.PatchPlan, error) {
	restart := false
	for _, op := range ops {
		restart = restart || op.Path == "host" || op.Path == "port"
	}
	f.planned = domainconfig.PatchPlan{Snapshot: snapshot, Operations: ops, RestartRequired: restart, Diff: "redacted diff"}
	return f.planned, nil
}
func (f *classifyingConfig) Apply(context.Context, domainconfig.PatchPlan) (domainconfig.PatchResult, error) {
	return domainconfig.PatchResult{RestartRequired: f.planned.RestartRequired}, nil
}
func (f *classifyingConfig) Validate(context.Context, domainconfig.ConfigSnapshot) []domainconfig.Diagnostic {
	return nil
}

type fakeConfigMaintenance struct {
	editRequest ConfigEditRequest
	editResult  ConfigEditResult
}

func (*fakeConfigMaintenance) Backup(context.Context, string) (string, error) { return "backup", nil }
func (*fakeConfigMaintenance) Restore(context.Context, string, string) (any, error) {
	return map[string]any{"restored": true}, nil
}
func (f *fakeConfigMaintenance) Edit(_ context.Context, request ConfigEditRequest) (ConfigEditResult, error) {
	f.editRequest = request
	ok, err := request.Confirm("redacted diff")
	if err != nil {
		return ConfigEditResult{}, err
	}
	if !ok {
		return ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "edit canceled")
	}
	return f.editResult, nil
}

type fakePMuxConfigMaintenance struct {
	backupID string
	plan     PMuxConfigRestorePlan
	restored bool
}

func (f *fakePMuxConfigMaintenance) BackupPMux(context.Context) (string, error) {
	return f.backupID, nil
}
func (f *fakePMuxConfigMaintenance) PlanRestorePMux(context.Context, string, state.Config) (PMuxConfigRestorePlan, error) {
	return f.plan, nil
}
func (f *fakePMuxConfigMaintenance) RestorePMux(context.Context, PMuxConfigRestorePlan) error {
	f.restored = true
	return nil
}

func configuredState(installation state.Installation) *memoryStore {
	return &memoryStore{state: state.State{Version: state.SchemaVersion, Installations: []state.Installation{installation}}}
}

func TestInteractiveServiceMutationsRequireExactTypedPhrase(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation Operation
		phrase    string
		prompt    string
		assert    func(*testing.T, *fakeService)
	}{
		{name: "stop", operation: OpServiceStop, phrase: "stop", prompt: "Stop CLIProxyAPI? Active client requests may fail. Type stop to confirm: ", assert: func(t *testing.T, service *fakeService) {
			if service.stops != 1 {
				t.Fatalf("stop calls = %d", service.stops)
			}
		}},
		{name: "restart", operation: OpServiceRestart, phrase: "restart", prompt: "Restart CLIProxyAPI? Active client requests may be interrupted. Type restart to confirm: ", assert: func(t *testing.T, service *fakeService) {
			if service.restarts != 1 {
				t.Fatalf("restart calls = %d", service.restarts)
			}
		}},
		{name: "uninstall", operation: OpServiceUninstall, phrase: "uninstall", prompt: "Uninstall the PMux service definition? The binary, config, and credentials will be kept. Type uninstall to confirm: ", assert: func(t *testing.T, service *fakeService) {
			if service.uninstalls != 1 {
				t.Fatalf("uninstall calls = %d", service.uninstalls)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			installation := state.Installation{ID: "default", Kind: "managed", ServiceBackend: "foreground"}
			manager := &fakeService{status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground, State: domainservice.ServiceRunning}}
			output := &bytes.Buffer{}
			router, err := NewRouter(Dependencies{
				Roots: testRoots(), Store: configuredState(installation), Input: strings.NewReader(test.phrase + "\n"), Output: output,
				Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
					return manager, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := router.Execute(context.Background(), Invocation{Operation: test.operation, Interactive: true}, nil); err != nil {
				t.Fatalf("confirmed mutation: %v", err)
			}
			test.assert(t, manager)
			if output.String() != test.prompt {
				t.Fatalf("prompt = %q, want %q", output.String(), test.prompt)
			}
		})
		t.Run(test.name+" rejects wrong phrase", func(t *testing.T) {
			installation := state.Installation{ID: "default", Kind: "managed", ServiceBackend: "foreground"}
			manager := &fakeService{status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground, State: domainservice.ServiceRunning}}
			router, err := NewRouter(Dependencies{
				Roots: testRoots(), Store: configuredState(installation), Input: strings.NewReader("wrong\n"),
				Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
					return manager, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := router.Execute(context.Background(), Invocation{Operation: test.operation, Interactive: true}, nil); pmuxerr.ExitCode(err) != 10 {
				t.Fatalf("wrong-phrase error = %v", err)
			}
			if manager.stops != 0 || manager.restarts != 0 || manager.uninstalls != 0 {
				t.Fatalf("mutation ran after wrong phrase: %#v", manager)
			}
		})
	}
}

func TestStoppedServiceStopRemainsIdempotentWithoutPrompt(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed", ServiceBackend: "foreground"}
	manager := &fakeService{status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground, State: domainservice.ServiceStopped}}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: configuredState(installation), Input: strings.NewReader(""),
		Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
			return manager, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpServiceStop, Interactive: true}, nil); err != nil {
		t.Fatal(err)
	}
	if manager.stops != 1 {
		t.Fatalf("stop calls = %d", manager.stops)
	}
}

func TestServiceLogsFilterBoundRedactOutputAndTerminalEvent(t *testing.T) {
	root := t.TempDir()
	installation := state.Installation{ID: "default", Kind: "managed", Host: "127.0.0.1", Port: 8317, ServiceBackend: "foreground"}
	serviceManager := &fakeService{
		status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground},
		logText: "2026-07-20T11:00:00Z INFO old\n" +
			"2026-07-20T12:01:00Z ERROR previous\n" +
			"2026-07-20T12:02:00Z ERROR newest Bearer provider-canary exact-proxy-canary exact-management-canary private_key=-----BEGIN_PRIVATE_KEY----- QWxhZGRpbjpPcGVuU2VzYW1lU2VlZGVkUHJpdmF0ZUtleU1hdGVyaWFs\n",
	}
	router, err := NewRouter(Dependencies{
		Roots: domainplatform.Roots{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"), Cache: filepath.Join(root, "cache"), Data: filepath.Join(root, "data")},
		Store: configuredState(installation),
		Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
			return serviceManager, nil
		},
		KnownSecrets: func(context.Context, state.Installation) ([][]byte, error) {
			return [][]byte{[]byte("exact-proxy-canary"), []byte("exact-management-canary")}, nil
		},
		Now: func() time.Time { return time.Date(2026, 7, 20, 12, 3, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "export", "logs.ndjson")
	var events []Event
	result, err := router.Execute(context.Background(), Invocation{Operation: OpServiceLogs, Options: map[string]any{
		"source": "service", "level": "error", "lines": 1, "since": "2026-07-20T12:00:00Z", "output": output,
	}}, func(event Event) error { events = append(events, event); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Streamed || len(events) != 2 || events[0].Type != "log" || events[1].Type != "complete" {
		t.Fatalf("events=%#v result=%#v", events, result)
	}
	if events[0].InstanceID != "default" || events[1].InstanceID != "default" {
		t.Fatalf("stream events lack instance identity: %#v", events)
	}
	if serviceManager.logTail != 1 || serviceManager.logFollow {
		t.Fatalf("log arguments tail=%d follow=%t", serviceManager.logTail, serviceManager.logFollow)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if leaked := firstCanary(string(body), "provider-canary", "exact-proxy-canary", "exact-management-canary", "QWxhZGRpbjpPcGVuU2VzYW1lU2VlZGVkUHJpdmF0ZUtleU1hdGVyaWFs"); leaked != "" || !strings.Contains(string(body), "newest") {
		t.Fatalf("unsafe or unbounded output (leaked %q): %s", leaked, body)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("output stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("output permissions=%v", info.Mode().Perm())
	}
}

func TestServiceLogClearRequiresAuthorizationAndOnlyProxySource(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed"}
	client := &logManagement{}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: configuredState(installation),
		Management: func(context.Context, state.Installation) (management.ManagementClient, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invocation := range []Invocation{
		{Operation: OpServiceLogs, Options: map[string]any{"clear": "audit"}, Yes: true},
		{Operation: OpServiceLogs, Options: map[string]any{"clear": "proxy"}},
	} {
		if _, err := router.Execute(context.Background(), invocation, nil); err == nil {
			t.Fatalf("clear invocation unexpectedly succeeded: %#v", invocation)
		}
	}
	if client.deleted {
		t.Fatal("unauthorized or forbidden clear reached upstream")
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpServiceLogs, Options: map[string]any{"clear": "proxy"}, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if !client.deleted {
		t.Fatal("authorized proxy clear did not reach upstream")
	}
}

func TestServiceLogsKnownSecretLoadFailureCreatesNoOutput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	installation := state.Installation{ID: "default", Kind: "managed", Host: "127.0.0.1", Port: 8317, ServiceBackend: "foreground"}
	serviceManager := &fakeService{
		status:  domainservice.ServiceStatus{Backend: domainservice.BackendForeground},
		logText: "2026-07-20T12:02:00Z ERROR secret material\n",
	}
	output := filepath.Join(root, "logs.ndjson")
	router, err := NewRouter(Dependencies{
		Roots: domainplatform.Roots{
			Config: filepath.Join(root, "config"),
			State:  filepath.Join(root, "state"),
			Cache:  filepath.Join(root, "cache"),
			Data:   filepath.Join(root, "data"),
		},
		Store: configuredState(installation),
		Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
			return serviceManager, nil
		},
		KnownSecrets: func(context.Context, state.Installation) ([][]byte, error) {
			return nil, errors.New("secret source unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Execute(context.Background(), Invocation{
		Operation: OpServiceLogs,
		Options:   map[string]any{"source": "service", "output": output},
	}, nil)
	if err == nil {
		t.Fatal("expected redaction dependency failure")
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output exists after fail-closed redaction setup: %v", statErr)
	}
}

func TestConfigEffectiveMetadataAndRestartClassification(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed", ConfigPath: "/tmp/config.yaml", ServiceBackend: "foreground"}
	configAdapter := &classifyingConfig{snapshot: domainconfig.ConfigSnapshot{Path: installation.ConfigPath, Config: domainconfig.Config{Host: "127.0.0.1", Port: 8317}}}
	serviceManager := &fakeService{status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground, State: domainservice.ServiceRunning}}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: configuredState(installation),
		Management: func(context.Context, state.Installation) (management.ManagementClient, error) {
			return &logManagement{}, nil
		},
		Configs: func(context.Context, state.Installation) (domainconfig.ConfigFile, error) { return configAdapter, nil },
		Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
			return serviceManager, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	shown, err := router.Execute(context.Background(), Invocation{Operation: OpConfigShow, Options: map[string]any{"scope": "proxy", "effective": true}}, nil)
	if err != nil || !strings.Contains(toJSON(shown.Data), `"restart_required"`) || !strings.Contains(toJSON(shown.Data), `"source"`) {
		t.Fatalf("effective=%s err=%v", toJSON(shown.Data), err)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpConfigSet, Arguments: []string{"port", "9000"}, Options: map[string]any{"scope": "proxy", "restart": true}, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if serviceManager.restarts != 1 {
		t.Fatalf("restart count=%d, want 1", serviceManager.restarts)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpConfigSet, Arguments: []string{"ws-auth", "true"}, Options: map[string]any{"scope": "proxy", "restart": true}, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if serviceManager.restarts != 1 {
		t.Fatalf("hot-reloadable setting caused restart count=%d", serviceManager.restarts)
	}
}

func TestManagementScalarSetVerifiesAndCompensates(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed"}
	client := &logManagement{settings: map[management.SettingName]management.SettingValue{
		"request-retry": management.SettingValue(`{"value":1}`),
	}}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: configuredState(installation),
		Management: func(context.Context, state.Installation) (management.ManagementClient, error) { return client, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.Execute(context.Background(), Invocation{
		Operation: OpConfigSet, Arguments: []string{"request-retry", "3"}, Options: map[string]any{"scope": "proxy"}, Yes: true,
	}, nil)
	if err != nil || !strings.Contains(toJSON(result.Data), `"transport":"management"`) || len(client.putHistory) != 1 {
		t.Fatalf("set result=%s history=%q err=%v", toJSON(result.Data), client.putHistory, err)
	}
	client.settings["request-retry"] = management.SettingValue(`{"value":1}`)
	client.putHistory = nil
	client.mismatchOnce = true
	if _, err := router.Execute(context.Background(), Invocation{
		Operation: OpConfigSet, Arguments: []string{"request-retry", "4"}, Options: map[string]any{"scope": "proxy"}, Yes: true,
	}, nil); err == nil {
		t.Fatal("mismatched management read-back unexpectedly succeeded")
	}
	if len(client.putHistory) != 2 || !jsonEquivalent(client.settings["request-retry"], management.SettingValue(`{"value":1}`)) {
		t.Fatalf("compensation history=%q setting=%s", client.putHistory, client.settings["request-retry"])
	}
	if _, err := router.Execute(context.Background(), Invocation{
		Operation: OpConfigSet, Arguments: []string{"request-retry", "-1"}, Options: map[string]any{"scope": "proxy"}, Yes: true,
	}, nil); err == nil {
		t.Fatal("invalid scalar value unexpectedly reached management")
	}
}

func TestPMuxBackupRestoreWorksWithoutConfiguredProxy(t *testing.T) {
	store := &memoryStore{}
	maintenance := &fakePMuxConfigMaintenance{
		backupID: "config.json.20260720T120000Z.abcdef12.bak",
		plan:     PMuxConfigRestorePlan{ID: "config.json.20260720T120000Z.abcdef12.bak", Config: state.Config{Version: state.SchemaVersion, Theme: "dark"}, Diff: "redacted"},
	}
	router, err := NewRouter(Dependencies{Roots: testRoots(), Store: store, PMuxConfig: maintenance})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Execute(context.Background(), Invocation{
		Operation: OpConfigBackup, Options: map[string]any{"scope": "pmux"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Execute(context.Background(), Invocation{
		Operation: OpConfigRestore, Arguments: []string{maintenance.backupID}, Options: map[string]any{"scope": "pmux"}, Yes: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if !maintenance.restored {
		t.Fatal("PMux settings restore was not dispatched")
	}
}

func TestPersistentSlotRequiresLiveModelAndRollsBackOnStateFailure(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed", Host: "127.0.0.1", Port: 8317}
	store := configuredState(installation)
	store.saveConfigErr = errors.New("state write failed")
	catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{ID: "live-exact", Available: true}}}
	launcher := &fakeLauncher{persistPlan: domainclient.PersistPlan{
		Path: "/tmp/settings.json", Before: []byte("before"), After: []byte("after"), Diff: "redacted",
	}}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store,
		Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
		Launcher: func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error) {
			return launcher, nil
		},
		Secrets: func(context.Context, state.Installation) ([]byte, error) { return []byte("sk-private-123456"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(model string) error {
		_, err := router.Execute(context.Background(), Invocation{
			Operation: OpConfigSet,
			Arguments: []string{"integrations.claude.persistent-models.opus", model},
			Options:   map[string]any{"scope": "pmux"}, Yes: true,
		}, nil)
		return err
	}
	if err := invoke("not-live"); err == nil || len(launcher.upserts) != 0 {
		t.Fatalf("unavailable model err=%v upserts=%d", err, len(launcher.upserts))
	}
	if err := invoke("live-exact"); err == nil {
		t.Fatal("state failure unexpectedly succeeded")
	}
	if len(launcher.upserts) != 2 {
		t.Fatalf("upserts=%d want commit+rollback", len(launcher.upserts))
	}
	reverse := launcher.upserts[1]
	if string(reverse.Before) != "after" || string(reverse.After) != "before" {
		t.Fatalf("rollback plan=%#v", reverse)
	}
}

func TestConfigEditRequiresInteractiveConfirmationAndHonorsRestart(t *testing.T) {
	installation := state.Installation{ID: "default", Kind: "managed", ConfigPath: "/tmp/config.yaml", ServiceBackend: "foreground"}
	maintenance := &fakeConfigMaintenance{editResult: ConfigEditResult{Path: installation.ConfigPath, Diff: "redacted", RestartRequired: true}}
	serviceManager := &fakeService{status: domainservice.ServiceStatus{Backend: domainservice.BackendForeground}}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: configuredState(installation), ConfigFiles: maintenance,
		Services: func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error) {
			return serviceManager, nil
		},
		Input: strings.NewReader("write\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpConfigEdit, Options: map[string]any{"scope": "proxy", "editor": "/bin/editor"}}, nil); err == nil {
		t.Fatal("noninteractive config edit unexpectedly succeeded")
	}
	if _, err := router.Execute(context.Background(), Invocation{Operation: OpConfigEdit, Options: map[string]any{"scope": "proxy", "editor": "/bin/editor", "restart": true}, Interactive: true}, nil); err != nil {
		t.Fatal(err)
	}
	if maintenance.editRequest.Editor != "/bin/editor" || serviceManager.restarts != 1 {
		t.Fatalf("request=%#v restarts=%d", maintenance.editRequest, serviceManager.restarts)
	}
}

func firstCanary(body string, canaries ...string) string {
	for _, canary := range canaries {
		if strings.Contains(body, canary) {
			return canary
		}
	}
	return ""
}
