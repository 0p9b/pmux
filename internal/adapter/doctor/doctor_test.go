package doctor

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	domaindoctor "github.com/0p9b/pmux/internal/domain/doctor"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type seedSource struct {
	binary BinaryFact
	config AbsoluteConfigFact
	safe SafeModeFact
	permissions PermissionsFact
	service ServiceFact
	health HealthFact
	providers ProviderFact
	models ModelsFact
	claude ClaudeFact
	compat CompatibilityFact
	update UpdateStateFact
	port PortFact
	management ManagementLocalFact
	exposure ExposureFact
	stateLock StateLockFact
}

func healthySource() *seedSource {
	return &seedSource{
		binary: BinaryFact{Path: "/opt/pmux/cli-proxy-api", Exists: true, Executable: true, ArchitectureOK: true, Managed: true, ChecksumOK: true, Version: "7.2.92"},
		config: AbsoluteConfigFact{ConfigPath: "/opt/pmux/config.yaml", ConfigReadable: true, ConfigParsed: true, WSAuthEnabled: true, ArgvUsesAbsolutePath: true, RuntimeDir: "/opt/pmux/runtime"},
		safe: SafeModeFact{HTTPStatus: 200, Authenticated: true},
		permissions: PermissionsFact{Targets: []PermissionTarget{{Path: "/opt/pmux/config.yaml", Secure: true}, {Path: "/opt/pmux/auth", Auth: true, Secure: true}}},
		service: ServiceFact{Backend: "foreground", Installed: true, Running: true, DefinitionOwned: true, IdentityMatches: true, DefinitionUsesConfig: true, EnvironmentScrubbed: true, RuntimeDirClean: true},
		health: HealthFact{HTTPStatus: 200, Version: "7.2.92", Endpoint: "http://127.0.0.1:8317/healthz", LatencyMS: 10},
		providers: ProviderFact{Configured: 1, Usable: 1},
		models: ModelsFact{DiscoverySucceeded: true, Count: 2, Source: "management"},
		claude: ClaudeFact{Found: true, VersionKnown: true, Supported: true, Version: "2.1.215", Path: "/usr/bin/claude"},
		compat: CompatibilityFact{VersionKnown: true, DetectedVersion: "7.2.92", MinimumVersion: "7.2.91", FloorSatisfied: true},
		update: UpdateStateFact{},
		port: PortFact{Host: "127.0.0.1", Port: 8317, Listening: true, ExpectedOwner: true, Owner: "cli-proxy-api"},
		management: ManagementLocalFact{Required: true, Enabled: true, Authenticated: true, ControlPanelDisabled: true},
		exposure: ExposureFact{Host: "127.0.0.1", Loopback: true, ManagementLocal: true},
		stateLock: StateLockFact{ReadOnlyStateAccessible: true, MutationLockAvailable: true},
	}
}

func (s *seedSource) Binary(context.Context) (BinaryFact, error) { return s.binary, nil }
func (s *seedSource) AbsoluteConfig(context.Context) (AbsoluteConfigFact, error) { return s.config, nil }
func (s *seedSource) SafeMode(context.Context) (SafeModeFact, error) { return s.safe, nil }
func (s *seedSource) Permissions(context.Context) (PermissionsFact, error) { return s.permissions, nil }
func (s *seedSource) Service(context.Context) (ServiceFact, error) { return s.service, nil }
func (s *seedSource) Health(context.Context) (HealthFact, error) { return s.health, nil }
func (s *seedSource) Providers(context.Context) (ProviderFact, error) { return s.providers, nil }
func (s *seedSource) Models(context.Context) (ModelsFact, error) { return s.models, nil }
func (s *seedSource) Claude(context.Context) (ClaudeFact, error) { return s.claude, nil }
func (s *seedSource) Compatibility(context.Context) (CompatibilityFact, error) { return s.compat, nil }
func (s *seedSource) UpdateState(context.Context) (UpdateStateFact, error) { return s.update, nil }
func (s *seedSource) Port(context.Context) (PortFact, error) { return s.port, nil }
func (s *seedSource) ManagementLocal(context.Context) (ManagementLocalFact, error) { return s.management, nil }
func (s *seedSource) Exposure(context.Context) (ExposureFact, error) { return s.exposure, nil }
func (s *seedSource) StateLock(context.Context) (StateLockFact, error) { return s.stateLock, nil }

func registryFor(t *testing.T, source Source) *Registry {
	t.Helper()
	r, err := NewDefaultRegistry(source)
	if err != nil { t.Fatal(err) }
	return r
}

func TestReportExactJSONSchema(t *testing.T) {
	report, err := (Runner{Registry: registryFor(t, healthySource())}).Run(context.Background())
	if err != nil { t.Fatal(err) }
	encoded, err := report.JSON()
	if err != nil { t.Fatal(err) }
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &root); err != nil { t.Fatal(err) }
	assertKeys(t, root, "checks", "summary")
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(root["checks"], &checks); err != nil { t.Fatal(err) }
	if len(checks) != 19 { t.Fatalf("got %d checks", len(checks)) }
	for _, check := range checks {
		assertKeys(t, check, "id", "status", "severity", "summary", "evidence", "repair")
		for _, obsolete := range []string{"name", "category", "repairable", "applied", "verified"} {
			if _, ok := check[obsolete]; ok { t.Fatalf("obsolete field %q present", obsolete) }
		}
		var repair map[string]json.RawMessage
		if err := json.Unmarshal(check["repair"], &repair); err != nil { t.Fatal(err) }
		assertKeys(t, repair, "available", "description", "destructive", "confirmation_required", "verification")
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(root["summary"], &summary); err != nil { t.Fatal(err) }
	assertKeys(t, summary, "passed", "warnings", "failed", "critical", "exit_code")
}

func assertKeys(t *testing.T, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object { got = append(got, key) }
	sort.Strings(got); sort.Strings(want)
	if !reflect.DeepEqual(got, want) { t.Fatalf("keys = %v, want %v", got, want) }
}

func TestWarningsExitZero(t *testing.T) {
	source := healthySource()
	source.health.Version = ""
	source.providers = ProviderFact{}
	source.models = ModelsFact{DiscoverySucceeded: true, Source: "management"}
	source.claude = ClaudeFact{}
	report, err := (Runner{Registry: registryFor(t, source)}).Run(context.Background(), CheckHealth, CheckProviders, CheckModels, CheckClaude)
	if err != nil { t.Fatal(err) }
	if report.Summary.Warnings != 4 || report.Summary.Failed != 0 || report.Summary.ExitCode != 0 { t.Fatalf("unexpected summary: %+v", report.Summary) }
}

func TestEverySeededFailureDetectedAndExitsSeven(t *testing.T) {
	source := healthySource()
	source.binary.Exists = false
	source.config.ArgvUsesAbsolutePath = false
	source.safe = SafeModeFact{HTTPStatus: 403, Header: "example-api-key", PlaceholderConfigured: true}
	source.permissions.Targets[0] = PermissionTarget{Path: "/opt/pmux/config.yaml", Secure: false, Detail: "mode 0644"}
	source.service.Running = false
	source.health.HTTPStatus = 503
	source.providers = ProviderFact{Configured: 1, Unavailable: 1}
	source.models = ModelsFact{DiscoverySucceeded: false, Cause: "management unavailable"}
	source.claude = ClaudeFact{Found: true, VersionKnown: true, Version: "1.9.0"}
	source.compat.FloorSatisfied = false
	source.update.UnexpectedEgress = true
	report, err := (Runner{Registry: registryFor(t, source)}).Run(context.Background())
	if err != nil { t.Fatal(err) }
	if report.Summary.ExitCode != 7 { t.Fatalf("exit = %d", report.Summary.ExitCode) }
	if report.Summary.Failed != 5 || report.Summary.Warnings != 1 || report.Summary.Critical != 5 { t.Fatalf("unexpected summary: %+v", report.Summary) }
	skipped := 0
	for _, check := range report.Checks {
		if check.Status == domaindoctor.StatusSkip { skipped++ }
	}
	if skipped != 9 { t.Fatalf("skipped dependency checks = %d, want 9", skipped) }
}
func TestExpandedRegistryChecksEvaluateRealFacts(t *testing.T) {
	cases := []struct {
		id string
		status domaindoctor.Status
		mutate func(*seedSource)
	}{
		{CheckConfigParse, domaindoctor.StatusFail, func(s *seedSource) { s.config.ConfigReadable = false }},
		{CheckWSAuth, domaindoctor.StatusWarn, func(s *seedSource) { s.config.WSAuthEnabled = false }},
		{CheckServiceDefinition, domaindoctor.StatusFail, func(s *seedSource) { s.service.Required = true; s.service.Installed = false }},
		{CheckPort, domaindoctor.StatusFail, func(s *seedSource) { s.port.ExpectedOwner = false }},
		{CheckManagementLocal, domaindoctor.StatusFail, func(s *seedSource) { s.management.AllowRemote = true }},
		{CheckExposure, domaindoctor.StatusFail, func(s *seedSource) { s.exposure = ExposureFact{Host: "0.0.0.0", ManagementLocal: true} }},
		{CheckAuthPermissions, domaindoctor.StatusFail, func(s *seedSource) { s.permissions.Targets[1].Secure = false }},
		{CheckStateLock, domaindoctor.StatusFail, func(s *seedSource) { s.stateLock.ReadOnlyStateAccessible = false }},
	}
	for _, test := range cases {
		t.Run(test.id, func(t *testing.T) {
			source := healthySource()
			test.mutate(source)
			report, err := (Runner{Registry: registryFor(t, source)}).Run(context.Background(), test.id)
			if err != nil { t.Fatal(err) }
			var target *domaindoctor.CheckResult
			for index := range report.Checks {
				if report.Checks[index].ID == test.id { target = &report.Checks[index]; break }
			}
			if target == nil || target.Status != test.status { t.Fatalf("target=%+v, want status %s", target, test.status) }
		})
	}
}

func TestRequestedCheckRunsPrerequisitesAndSkipsDependent(t *testing.T) {
	source := healthySource()
	source.service.Running = false
	report, err := (Runner{Registry: registryFor(t, source)}).Run(context.Background(), CheckHealth)
	if err != nil { t.Fatal(err) }
	gotIDs := make([]string, 0, len(report.Checks))
	for _, result := range report.Checks { gotIDs = append(gotIDs, result.ID) }
	wantIDs := []string{CheckConfigParse, CheckAbsoluteConfig, CheckServiceDefinition, CheckService, CheckPort, CheckHealth}
	if !reflect.DeepEqual(gotIDs, wantIDs) { t.Fatalf("check order=%v, want %v", gotIDs, wantIDs) }
	if report.Checks[len(report.Checks)-1].Status != domaindoctor.StatusSkip {
		t.Fatalf("health status=%s, want skip", report.Checks[len(report.Checks)-1].Status)
	}
}


func TestCriticalResultExitsSeven(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterCheck(checkFunc{id: "CRITICAL", title: "critical", run: func(context.Context) domaindoctor.CheckResult {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityCritical, "critical condition", nil, noRepair())
	}}); err != nil { t.Fatal(err) }
	report, err := (Runner{Registry: r}).Run(context.Background())
	if err != nil { t.Fatal(err) }
	if report.Checks[0].Status != domaindoctor.StatusFail || report.Summary.Critical != 1 || report.Summary.ExitCode != 7 {
		t.Fatalf("critical result did not fail: %+v", report)
	}
}

func TestRunnerRedactsKnownSecretsAndControlSequences(t *testing.T) {
	secret := "sk-super-secret-canary"
	r := NewRegistry()
	if err := r.RegisterCheck(checkFunc{id: "SECRET", title: "secret", run: func(context.Context) domaindoctor.CheckResult {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "bad \x1b[31m"+secret, []string{secret}, noRepair())
	}}); err != nil { t.Fatal(err) }
	report, err := (Runner{Registry: r, KnownSecrets: []string{secret}}).Run(context.Background())
	if err != nil { t.Fatal(err) }
	encoded, _ := report.JSON()
	if string(encoded) == "" || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "\\u001b") { t.Fatalf("unsafe report: %s", encoded) }
}

type mutableCheck struct { id string; pass *bool }
func (c mutableCheck) ID() string { return c.id }
func (c mutableCheck) Title() string { return c.id }
func (c mutableCheck) Run(context.Context) domaindoctor.CheckResult {
	if *c.pass { return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "pass", nil, noRepair()) }
	return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "fail", nil, repair("fix", "pass", false))
}

type mutableFix struct { id, check string; value *bool; verify bool; events *[]string }
func (f *mutableFix) ID() string { return f.id }
func (f *mutableFix) CheckID() string { return f.check }
func (f *mutableFix) Apply(_ context.Context, dry bool) (domaindoctor.FixResult, error) {
	if dry { *f.events = append(*f.events, "preview:"+f.id); return domaindoctor.FixResult{CheckID: f.check, Summary: "preview"}, nil }
	*f.events = append(*f.events, "apply:"+f.id); *f.value = true
	return domaindoctor.FixResult{CheckID: f.check, Changed: true, Verified: f.verify}, nil
}
func (f *mutableFix) Rollback(context.Context) error { *f.events = append(*f.events, "rollback:"+f.id); *f.value = false; return nil }

func TestCheckRunnerIsReadOnlyEvenWhenFixRegistered(t *testing.T) {
	passed := false
	events := []string{}
	r := NewRegistry()
	if err := r.RegisterCheck(mutableCheck{"A", &passed}); err != nil { t.Fatal(err) }
	if err := r.RegisterFix(&mutableFix{"fix-a", "A", &passed, true, &events}); err != nil { t.Fatal(err) }
	report, err := (Runner{Registry: r}).Run(context.Background())
	if err != nil { t.Fatal(err) }
	if report.Summary.ExitCode != 7 || passed || len(events) != 0 {
		t.Fatalf("read-only run mutated state: report=%+v passed=%t events=%v", report, passed, events)
	}
}
func TestRunnerTopologicallyOrdersPrerequisitesAndRejectsCycles(t *testing.T) {
	r := NewRegistry()
	child := checkFunc{id: "CHILD", title: "child", run: func(context.Context) domaindoctor.CheckResult {
		return result(domaindoctor.StatusPass, domaindoctor.SeverityInfo, "child", nil, noRepair())
	}}
	parent := checkFunc{id: "PARENT", title: "parent", run: func(context.Context) domaindoctor.CheckResult {
		return result(domaindoctor.StatusPass, domaindoctor.SeverityInfo, "parent", nil, noRepair())
	}}
	if err := r.RegisterCheck(child); err != nil { t.Fatal(err) }
	if err := r.RegisterCheck(parent); err != nil { t.Fatal(err) }
	if err := r.RegisterPrerequisites("CHILD", "PARENT"); err != nil { t.Fatal(err) }
	report, err := (Runner{Registry: r}).Run(context.Background(), "CHILD")
	if err != nil { t.Fatal(err) }
	if got := []string{report.Checks[0].ID, report.Checks[1].ID}; !reflect.DeepEqual(got, []string{"PARENT", "CHILD"}) {
		t.Fatalf("execution order=%v", got)
	}
	if err := r.RegisterPrerequisites("PARENT", "CHILD"); err == nil {
		t.Fatal("prerequisite cycle unexpectedly accepted")
	}
	if got := r.Prerequisites("CHILD"); !reflect.DeepEqual(got, []string{"PARENT"}) {
		t.Fatalf("failed cycle registration changed existing prerequisites: %v", got)
	}
}


func TestFixRunnerDryRunConfirmationAndRollback(t *testing.T) {
	first, second := false, false
	events := []string{}
	r := NewRegistry()
	for _, check := range []domaindoctor.Check{mutableCheck{"A", &first}, mutableCheck{"B", &second}} { if err := r.RegisterCheck(check); err != nil { t.Fatal(err) } }
	for _, fix := range []domaindoctor.Fix{&mutableFix{"fix-a", "A", &first, true, &events}, &mutableFix{"fix-b", "B", &second, false, &events}} { if err := r.RegisterFix(fix); err != nil { t.Fatal(err) } }

	dry, err := (FixRunner{Registry: r}).Run(context.Background(), nil, FixOptions{DryRun: true})
	if err != nil { t.Fatal(err) }
	if first || second || len(dry.Results) != 0 || !reflect.DeepEqual(events, []string{"preview:fix-a", "preview:fix-b"}) { t.Fatalf("dry run mutated: first=%t second=%t events=%v", first, second, events) }

	events = nil
	_, err = (FixRunner{Registry: r}).Run(context.Background(), nil, FixOptions{})
	if pmuxerr.ExitCode(err) != 2 || first || second { t.Fatalf("missing confirmation: err=%v first=%t second=%t", err, first, second) }

	events = nil
	out, err := (FixRunner{Registry: r}).Run(context.Background(), nil, FixOptions{Yes: true})
	if err == nil { t.Fatal("expected verification failure") }
	if !out.RolledBack || first || second { t.Fatalf("rollback failed: %+v first=%t second=%t", out, first, second) }
	want := []string{"preview:fix-a", "preview:fix-b", "apply:fix-a", "apply:fix-b", "rollback:fix-b", "rollback:fix-a"}
	if !reflect.DeepEqual(events, want) { t.Fatalf("events = %v, want %v", events, want) }
	if pmuxerr.ExitCode(err) != 7 { t.Fatalf("verification failure exit = %d", pmuxerr.ExitCode(err)) }
}

func TestFixRunnerUsesConfirmationHookAndVerifies(t *testing.T) {
	passed := false
	events := []string{}
	r := NewRegistry()
	if err := r.RegisterCheck(mutableCheck{"A", &passed}); err != nil { t.Fatal(err) }
	if err := r.RegisterFix(&mutableFix{"fix-a", "A", &passed, true, &events}); err != nil { t.Fatal(err) }
	confirmed := false
	out, err := (FixRunner{Registry: r}).Run(context.Background(), []string{"A"}, FixOptions{Confirm: func(_ context.Context, plan FixPlan) (bool, error) {
		confirmed = true
		return reflect.DeepEqual(plan.CheckIDs, []string{"A"}), nil
	}})
	if err != nil { t.Fatal(err) }
	if !confirmed || !passed || out.RolledBack || out.Report.Summary.ExitCode != 0 {
		t.Fatalf("confirmation/verification failed: confirmed=%t passed=%t out=%+v", confirmed, passed, out)
	}
}
