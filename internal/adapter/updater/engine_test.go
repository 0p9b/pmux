package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/domain/update"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type fakeSource struct {
	release   Release
	archive   []byte
	checksums []byte
	resolves  int
	downloads int
}

func (f *fakeSource) Resolve(context.Context, update.Component, string) (Release, error) {
	f.resolves++
	return f.release, nil
}

func (f *fakeSource) Download(_ context.Context, url, destination string) error {
	f.downloads++
	var body []byte
	switch url {
	case f.release.ArchiveURL:
		body = f.archive
	case f.release.ChecksumsURL:
		body = f.checksums
	default:
		return errors.New("unexpected URL")
	}
	return os.WriteFile(destination, body, 0o600)
}

type fakeService struct {
	state      service.ServiceState
	startCalls int
	stopCalls  int
	failStart  int
	failStop   int
}

func (f *fakeService) Status(context.Context) (service.ServiceStatus, error) {
	return service.ServiceStatus{Backend: service.BackendForeground, State: f.state}, nil
}
func (f *fakeService) Start(context.Context) error {
	f.startCalls++
	if f.failStart > 0 {
		f.failStart--
		return errors.New("injected start failure")
	}
	f.state = service.ServiceRunning
	return nil
}
func (f *fakeService) Stop(context.Context, time.Duration) error {
	f.stopCalls++
	if f.failStop > 0 {
		f.failStop--
		return errors.New("injected stop failure")
	}
	f.state = service.ServiceStopped
	return nil
}

type fakeProxyVerifier struct {
	healthCalls int
	authCalls   int
	modelCalls  int
	models      []string
	failHealth  int
	failAuth    int
	failModels  int
}

func (f *fakeProxyVerifier) Health(context.Context) error {
	f.healthCalls++
	if f.failHealth > 0 {
		f.failHealth--
		return errors.New("injected health failure")
	}
	return nil
}
func (f *fakeProxyVerifier) Authenticated(context.Context) error {
	f.authCalls++
	if f.failAuth > 0 {
		f.failAuth--
		return errors.New("injected authentication failure")
	}
	return nil
}
func (f *fakeProxyVerifier) Models(context.Context) ([]string, error) {
	f.modelCalls++
	if f.failModels > 0 {
		f.failModels--
		return nil, errors.New("injected model failure")
	}
	return f.models, nil
}

type fakeSelfVerifier struct {
	preCalls  int
	postCalls int
	failPre   bool
	failPost  bool
}

func (f *fakeSelfVerifier) Preflight(context.Context, string, string) error {
	f.preCalls++
	if f.failPre {
		return errors.New("injected preflight failure")
	}
	return nil
}
func (f *fakeSelfVerifier) Postflight(context.Context, string, string) error {
	f.postCalls++
	if f.failPost && f.postCalls == 1 {
		return errors.New("injected postflight failure")
	}
	return nil
}

func TestEngineDoesNoNetworkUntilExplicitMethod(t *testing.T) {
	source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
	engine := New(source, nil, nil, nil)
	if source.resolves != 0 || source.downloads != 0 {
		t.Fatal("constructor performed release I/O")
	}
	got, err := engine.Check(context.Background(), CheckRequest{Component: update.Proxy, CurrentVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Available != "2.0.0" || source.resolves != 1 || source.downloads != 0 {
		t.Fatalf("check result=%+v resolves=%d downloads=%d", got, source.resolves, source.downloads)
	}
}

func TestUpdateRefusesForeignOwnershipBeforeNetwork(t *testing.T) {
	cases := []struct {
		name      string
		ownership Ownership
		component update.Component
	}{
		{name: "adopted proxy", ownership: OwnershipAdopted, component: update.Proxy},
		{name: "package managed self", ownership: OwnershipPackageManaged, component: update.Self},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := validSource(t, tc.component, "2.0.0", executableName(tc.component))
			engine := New(source, nil, nil, nil)
			var err error
			if tc.component == update.Proxy {
				_, err = engine.UpdateProxy(context.Background(), ProxyRequest{Ownership: tc.ownership})
			} else {
				_, err = engine.UpdateSelf(context.Background(), SelfRequest{Ownership: tc.ownership})
			}
			assertCode(t, err, pmuxerr.ConfigMutationConflict)
			if source.resolves != 0 || source.downloads != 0 {
				t.Fatalf("refused update performed release I/O: resolves=%d downloads=%d", source.resolves, source.downloads)
			}
		})
	}
}

func TestChecksumFailurePrecedesExtraction(t *testing.T) {
	cases := []struct {
		name      string
		checksums func(*fakeSource) []byte
	}{
		{name: "missing", checksums: func(*fakeSource) []byte { return []byte("00  another-file.tar.gz\n") }},
		{name: "mismatch", checksums: func(f *fakeSource) []byte { return []byte(strings.Repeat("0", 64) + "  " + f.release.ArchiveName + "\n") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, pointer, oldTarget := proxyLayout(t)
			source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
			source.checksums = tc.checksums(source)
			var stages []Stage
			engine := New(source, &fakeService{state: service.ServiceRunning}, &fakeProxyVerifier{}, nil, WithStageHook(func(stage Stage) error {
				stages = append(stages, stage)
				return nil
			}))
			_, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
			assertCode(t, err, pmuxerr.InstallIntegrityFailed)
			if containsStage(stages, StageExtract) {
				t.Fatal("extract stage ran after checksum failure")
			}
			assertPointer(t, pointer, oldTarget)
		})
	}
}

func TestExecutableMagicFailurePrecedesServiceAndActivation(t *testing.T) {
	root, pointer, oldTarget := proxyLayout(t)
	source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
	source.archive = archiveBytes(t, proxyExecutableName(), []byte("not an executable"))
	hash := sha256.Sum256(source.archive)
	source.checksums = []byte(hex.EncodeToString(hash[:]) + "  " + source.release.ArchiveName + "\n")
	svc := &fakeService{state: service.ServiceRunning}
	var stages []Stage
	engine := New(source, svc, &fakeProxyVerifier{}, nil, WithStageHook(func(stage Stage) error {
		stages = append(stages, stage)
		return nil
	}))
	_, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
	assertCode(t, err, pmuxerr.InstallUnsupportedTarget)
	assertPointer(t, pointer, oldTarget)
	if svc.stopCalls != 0 || containsStage(stages, StageInstallVersion) {
		t.Fatalf("invalid executable reached lifecycle/activation: stop=%d stages=%v", svc.stopCalls, stages)
	}
}

func TestProxyUpdateRollsBackEveryInjectedFailure(t *testing.T) {
	stages := []Stage{
		StageResolve,
		StageDownloadArchive,
		StageDownloadChecksums,
		StageVerifyChecksum,
		StageExtract,
		StageVerifyExecutable,
		StageStopService,
		StageInstallVersion,
		StageSwitchPointer,
		StageStartService,
		StageHealth,
		StageAuthenticate,
		StageModels,
	}
	for _, failedStage := range stages {
		t.Run(string(failedStage), func(t *testing.T) {
			root, pointer, oldTarget := proxyLayout(t)
			source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
			svc := &fakeService{state: service.ServiceRunning}
			verify := &fakeProxyVerifier{models: []string{"runtime-model"}}
			engine := New(source, svc, verify, nil, WithStageHook(func(stage Stage) error {
				if stage == failedStage {
					return errors.New("injected " + string(stage))
				}
				return nil
			}))
			_, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
			if err == nil {
				t.Fatal("injected failure unexpectedly succeeded")
			}
			assertPointer(t, pointer, oldTarget)
			if _, statErr := os.Stat(filepath.Join(root, "2.0.0")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed version remains after rollback: %v", statErr)
			}
			if failedStage == StageStartService || failedStage == StageHealth || failedStage == StageAuthenticate || failedStage == StageModels {
				if svc.state != service.ServiceRunning {
					t.Fatalf("prior service state was not restored: %s", svc.state)
				}
			}
		})
	}
}

func TestProxyVerifierFailuresRollback(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeProxyVerifier)
	}{
		{name: "health", mutate: func(v *fakeProxyVerifier) { v.failHealth = 1 }},
		{name: "authentication", mutate: func(v *fakeProxyVerifier) { v.failAuth = 1 }},
		{name: "models", mutate: func(v *fakeProxyVerifier) { v.failModels = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, pointer, oldTarget := proxyLayout(t)
			source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
			svc := &fakeService{state: service.ServiceRunning}
			verify := &fakeProxyVerifier{models: []string{"runtime-model"}}
			tc.mutate(verify)
			engine := New(source, svc, verify, nil)
			result, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
			assertCode(t, err, pmuxerr.InstallRollbackAttempted)
			if !result.RolledBack {
				t.Fatal("successful rollback was not reported")
			}
			assertPointer(t, pointer, oldTarget)
			if svc.state != service.ServiceRunning {
				t.Fatalf("prior service state was not restored: %s", svc.state)
			}
		})
	}
}

func TestProxyStartFailureRollsBack(t *testing.T) {
	root, pointer, oldTarget := proxyLayout(t)
	source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
	svc := &fakeService{state: service.ServiceRunning, failStart: 1}
	engine := New(source, svc, &fakeProxyVerifier{models: []string{"runtime-model"}}, nil)
	_, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
	assertCode(t, err, pmuxerr.InstallRollbackAttempted)
	assertPointer(t, pointer, oldTarget)
	if svc.state != service.ServiceRunning {
		t.Fatalf("prior service state was not restored: %s", svc.state)
	}
}

func TestEmptyModelsIsWarningSuccess(t *testing.T) {
	root, pointer, _ := proxyLayout(t)
	source := validSource(t, update.Proxy, "2.0.0", proxyExecutableName())
	svc := &fakeService{state: service.ServiceRunning}
	engine := New(source, svc, &fakeProxyVerifier{models: nil}, nil)
	result, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || len(result.Warnings) != 1 || result.Warnings[0] != "No effective credentials; model catalog is empty." {
		t.Fatalf("unexpected result: %+v", result)
	}
	assertPointer(t, pointer, filepath.Join(root, "2.0.0"))
	if svc.state != service.ServiceRunning {
		t.Fatalf("service not running after successful cutover: %s", svc.state)
	}
}

func TestSelfUpdatePreActivationFailuresLeaveExecutableUntouched(t *testing.T) {
	cases := []struct {
		name     string
		verifier *fakeSelfVerifier
		stage    Stage
	}{
		{name: "version preflight", verifier: &fakeSelfVerifier{failPre: true}},
		{name: "activation boundary", verifier: &fakeSelfVerifier{}, stage: StageActivate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			current := filepath.Join(root, selfExecutableName())
			old := []byte("old executable bytes")
			if err := os.WriteFile(current, old, 0o700); err != nil {
				t.Fatal(err)
			}
			source := validSource(t, update.Self, "2.0.0", selfExecutableName())
			engine := New(source, nil, nil, tc.verifier, WithStageHook(func(stage Stage) error {
				if tc.stage != "" && stage == tc.stage {
					return errors.New("injected activation failure")
				}
				return nil
			}))
			result, err := engine.UpdateSelf(context.Background(), SelfRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, ExecutablePath: current, Target: NativeTarget()})
			if err == nil {
				t.Fatal("injected failure unexpectedly succeeded")
			}
			if result.RolledBack {
				t.Fatal("pre-activation failure incorrectly reported rollback")
			}
			got, readErr := os.ReadFile(current)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, old) {
				t.Fatal("pre-activation failure changed current executable")
			}
		})
	}
}

func TestSelfUpdatePostflightFailureRestoresExecutable(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "pmux")
	old := []byte("old executable bytes")
	if err := os.WriteFile(current, old, 0o700); err != nil {
		t.Fatal(err)
	}
	source := validSource(t, update.Self, "2.0.0", selfExecutableName())
	verifier := &fakeSelfVerifier{failPost: true}
	engine := New(source, nil, nil, verifier)
	result, err := engine.UpdateSelf(context.Background(), SelfRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, ExecutablePath: current, Target: NativeTarget()})
	assertCode(t, err, pmuxerr.InstallRollbackAttempted)
	if !result.RolledBack {
		t.Fatal("successful self rollback was not reported")
	}
	got, readErr := os.ReadFile(current)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, old) {
		t.Fatal("self-update rollback did not restore original executable")
	}
	if verifier.postCalls != 2 {
		t.Fatalf("expected failed candidate and restored executable verification, got %d calls", verifier.postCalls)
	}
}

func TestSelfUpdateRetainsPreviousExecutable(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, selfExecutableName())
	old := []byte("old executable bytes")
	if err := os.WriteFile(current, old, 0o700); err != nil {
		t.Fatal(err)
	}
	source := validSource(t, update.Self, "2.0.0", selfExecutableName())
	verifier := &fakeSelfVerifier{}
	engine := New(source, nil, nil, verifier)
	result, err := engine.UpdateSelf(context.Background(), SelfRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, ExecutablePath: current, Target: NativeTarget()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || verifier.preCalls != 1 || verifier.postCalls != 1 {
		t.Fatalf("unexpected result or verification calls: result=%+v pre=%d post=%d", result, verifier.preCalls, verifier.postCalls)
	}
	previous, err := os.ReadFile(current + ".pmux-previous")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(previous, old) {
		t.Fatal("retained previous executable does not match original")
	}
	if err := verifyExecutable(current, NativeTarget()); err != nil {
		t.Fatalf("active replacement is not the verified executable: %v", err)
	}
}

func TestSameVersionDoesNotDownload(t *testing.T) {
	root, pointer, _ := proxyLayout(t)
	source := validSource(t, update.Proxy, "1.0.0", proxyExecutableName())
	engine := New(source, &fakeService{state: service.ServiceRunning}, &fakeProxyVerifier{}, nil)
	result, err := engine.UpdateProxy(context.Background(), ProxyRequest{CurrentVersion: "1.0.0", Ownership: OwnershipManaged, VersionsDir: root, CurrentPointer: pointer, Target: NativeTarget()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || source.resolves != 1 || source.downloads != 0 {
		t.Fatalf("same-version update result=%+v resolves=%d downloads=%d", result, source.resolves, source.downloads)
	}
}

func validSource(t *testing.T, component update.Component, version, executable string) *fakeSource {
	t.Helper()
	archiveName := "asset_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	archive := testExecutableArchive(t, executable)
	hash := sha256.Sum256(archive)
	return &fakeSource{
		release: Release{Component: component, Version: version, ArchiveName: archiveName, ArchiveURL: "memory://archive", ChecksumsURL: "memory://checksums", ExecutableName: executable},
		archive: archive,
		checksums: []byte(hex.EncodeToString(hash[:]) + "  " + archiveName + "\n"),
	}
}

func testExecutableArchive(t *testing.T, name string) []byte {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(tw, in); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
func archiveBytes(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}


func proxyLayout(t *testing.T) (versionsDir, pointer, oldTarget string) {
	t.Helper()
	versionsDir = filepath.Join(t.TempDir(), "versions")
	oldTarget = filepath.Join(versionsDir, "1.0.0")
	if err := os.MkdirAll(oldTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldTarget, proxyExecutableName()), []byte("old proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	pointer = filepath.Join(filepath.Dir(versionsDir), "current")
	if err := (nativePointerStore{}).Swap(pointer, oldTarget); err != nil {
		t.Fatal(err)
	}
	return versionsDir, pointer, oldTarget
}

func assertPointer(t *testing.T, pointer, expected string) {
	t.Helper()
	got, err := (nativePointerStore{}).Read(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if got != expected {
		t.Fatalf("current pointer=%q, want %q", got, expected)
	}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected pmux error, got %T: %v", err, err)
	}
	if typed.Code != code {
		t.Fatalf("error code=%s, want %s (%v)", typed.Code, code, err)
	}
}

func containsStage(stages []Stage, wanted Stage) bool {
	for _, stage := range stages {
		if stage == wanted {
			return true
		}
	}
	return false
}

func executableName(component update.Component) string {
	if component == update.Self {
		return selfExecutableName()
	}
	return proxyExecutableName()
}
func selfExecutableName() string {
	if runtime.GOOS == "windows" {
		return "pmux.exe"
	}
	return "pmux"
}
func proxyExecutableName() string {
	if runtime.GOOS == "windows" {
		return "cli-proxy-api.exe"
	}
	return "cli-proxy-api"
}
