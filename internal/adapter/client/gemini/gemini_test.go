package gemini

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type recordingRunner struct {
	output      []byte
	outputErr   error
	runErr      error
	outputCalls int
	runCalls    int
	process     Process
}

func (r *recordingRunner) Output(context.Context, string, ...string) ([]byte, error) {
	r.outputCalls++
	return append([]byte(nil), r.output...), r.outputErr
}

func (r *recordingRunner) Run(_ context.Context, process Process) error {
	r.runCalls++
	r.process = process
	return r.runErr
}

func launcherForTest(t *testing.T, runner Runner, preflight ModelPreflight, home string) *Launcher {
	t.Helper()
	return New(Options{
		LookPath: func(name string) (string, error) {
			if name != "gemini" {
				t.Fatalf("looked up %q, want gemini", name)
			}
			return filepath.Join(t.TempDir(), "gemini"), nil
		},
		Runner:         runner,
		Environment:    func() []string { return []string{"PATH=/bin", "KEEP=value"} },
		ModelPreflight: preflight,
		HomeDir:        home,
	})
}

func envMap(env []string) map[string]string {
	mapped := make(map[string]string, len(env))
	for _, value := range env {
		name, setting, _ := strings.Cut(value, "=")
		mapped[name] = setting
	}
	return mapped
}

func TestDetectVersionParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		output    string
		version   string
		supported bool
		wantError bool
	}{
		{name: "bare semver", output: "0.52.0\n", version: "0.52.0", supported: true},
		{name: "nightly", output: "0.52.0-nightly.20260715.gfa975395b\n", version: "0.52.0", supported: true},
		{name: "prefixed", output: "gemini v1.2.3\n", version: "1.2.3", supported: true},
		{name: "unparseable", output: "gemini current\n", version: "unknown", supported: false, wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte(test.output)}
			launcher := launcherForTest(t, runner, nil, t.TempDir())
			install, err := launcher.Detect(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("Detect error = %v, wantError %v", err, test.wantError)
			}
			if install.Version != test.version || install.Supported != test.supported {
				t.Fatalf("Detect = %+v, want version %q supported %v", install, test.version, test.supported)
			}
		})
	}
}

func TestDetectMissingExecutable(t *testing.T) {
	t.Parallel()
	launcher := New(Options{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }})
	_, err := launcher.Detect(context.Background())
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) {
		t.Fatalf("Detect error = %T %v, want *pmuxerr.Error", err, err)
	}
	if pmuxError.Code != pmuxerr.ClientBinaryMissing {
		t.Fatalf("error code = %q, want %q", pmuxError.Code, pmuxerr.ClientBinaryMissing)
	}
}

func TestInvalidLaunchesNeverSpawnGemini(t *testing.T) {
	t.Parallel()
	unavailable := errors.New("model unavailable")
	cases := []struct {
		name      string
		version   string
		client    domainclient.ClientID
		model     string
		args      []string
		home      string
		preflight ModelPreflight
		want      error
	}{
		{name: "wrong client", version: "0.52.0", client: domainclient.Claude, model: "exact-model"},
		{name: "missing model", version: "0.52.0", model: ""},
		{name: "model flag short", version: "0.52.0", model: "exact-model", args: []string{"-m", "other"}},
		{name: "model flag long", version: "0.52.0", model: "exact-model", args: []string{"--model", "other"}},
		{name: "model flag equals", version: "0.52.0", model: "exact-model", args: []string{"--model=other"}},
		{name: "unparseable client", version: "current", model: "exact-model", preflight: func(context.Context, string) error { return nil }},
		{name: "missing preflight", version: "0.52.0", model: "exact-model"},
		{name: "model unavailable", version: "0.52.0", model: "missing-model", preflight: func(_ context.Context, model string) error {
			if model != "missing-model" {
				t.Fatalf("preflight model = %q", model)
			}
			return unavailable
		}, want: unavailable},
		{name: "missing home", version: "0.52.0", model: "exact-model", home: "", preflight: func(context.Context, string) error { return nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			home := test.home
			if test.name != "missing home" {
				home = t.TempDir()
			}
			runner := &recordingRunner{output: []byte(test.version)}
			launcher := launcherForTest(t, runner, test.preflight, home)
			_, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{Client: test.client, Model: test.model, BaseURL: "http://127.0.0.1:8317", Token: "gm-1234567890123456", Args: test.args, WorkingDir: t.TempDir()})
			if err == nil {
				t.Fatal("Launch succeeded, want error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Launch error = %v, want %v", err, test.want)
			}
			if runner.runCalls != 0 {
				t.Fatalf("Gemini spawn count = %d, want 0", runner.runCalls)
			}
		})
	}
}

func TestLaunchUsesExactArgvEnvironmentCWDAndJSONSeparation(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: []byte("0.52.0-nightly.20260715.gfa975395b\n")}
	parent := []string{
		"PATH=/usr/bin", "KEEP=unchanged", "GEMINI_API_KEY=old", "GOOGLE_GEMINI_BASE_URL=https://wrong.invalid",
		"GOOGLE_VERTEX_BASE_URL=https://vertex.invalid", "GOOGLE_GENAI_USE_VERTEXAI=true", "GOOGLE_GENAI_USE_GCA=true",
		"GEMINI_MODEL=wrong", "GEMINI_CLI_HOME=/wrong", "GEMINI_API_KEY_AUTH_MECHANISM=oauth",
		"GEMINI_CLI_TRUST_WORKSPACE=false", "GEMINI_TELEMETRY_ENABLED=true", "GEMINI_CLI_CUSTOM_HEADERS=x",
	}
	parentBefore := append([]string(nil), parent...)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	stdin := strings.NewReader("input")
	workdir := t.TempDir()
	home := t.TempDir()
	var preflightModel string
	launcher := New(Options{
		LookPath:       func(string) (string, error) { return filepath.Join(workdir, "gemini"), nil },
		Runner:         runner,
		Environment:    func() []string { return parent },
		ModelPreflight: func(_ context.Context, model string) error { preflightModel = model; return nil },
		Stdin:          stdin, Stdout: stdout, Stderr: stderr, JSONMode: true,
		HomeDir: home,
	})
	spec := domainclient.LaunchSpec{
		Client: domainclient.Gemini, Model: "provider/gemini-3-pro",
		BaseURL: "http://127.0.0.1:8317", Token: "gm-1234567890123456",
		Args: []string{"--approval-mode", "auto_edit", "-p", "say hi"}, WorkingDir: workdir,
	}
	result, err := launcher.Launch(context.Background(), spec)
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	if result.ExitCode != 0 || result.Signal != "" {
		t.Fatalf("Launch result = %+v, want successful zero result", result)
	}
	if preflightModel != spec.Model {
		t.Fatalf("preflight model = %q, want %q", preflightModel, spec.Model)
	}
	wantArgs := []string{"--skip-trust", "-m", spec.Model, "--approval-mode", "auto_edit", "-p", "say hi"}
	if !reflect.DeepEqual(runner.process.Args, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", runner.process.Args, wantArgs)
	}
	if runner.process.Dir != workdir {
		t.Fatalf("cwd = %q, want %q", runner.process.Dir, workdir)
	}
	if runner.process.Stdin != stdin || runner.process.Stdout != stderr || runner.process.Stderr != stderr {
		t.Fatalf("stdio not routed for JSON separation: %#v", runner.process)
	}
	if !reflect.DeepEqual(parent, parentBefore) {
		t.Fatalf("parent environment mutated: got %#v want %#v", parent, parentBefore)
	}
	env := envMap(runner.process.Env)
	wantEnv := map[string]string{
		"PATH": "/usr/bin", "KEEP": "unchanged",
		apiKeyEnv: spec.Token, baseURLEnv: spec.BaseURL, authMechanismEnv: "bearer",
		modelEnv: spec.Model, cliHomeEnv: home, trustWorkspaceEnv: "true", telemetryEnv: "false",
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("child env = %#v, want %#v", env, wantEnv)
	}
	settings, err := os.ReadFile(filepath.Join(home, settingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(settings) != settingsJSON {
		t.Fatalf("settings = %q, want %q", settings, settingsJSON)
	}
}

func TestEnvValidation(t *testing.T) {
	t.Parallel()
	launcher := launcherForTest(t, &recordingRunner{}, nil, t.TempDir())
	for _, spec := range []domainclient.LaunchSpec{
		{Model: "exact", Token: "gm-1234567890123456"},
		{Model: "exact", BaseURL: "http://127.0.0.1:8317"},
	} {
		if _, err := launcher.Env(spec); err == nil {
			t.Fatalf("Env(%+v) succeeded, want error", spec)
		}
	}
}

func TestEnvRequiresHome(t *testing.T) {
	t.Parallel()
	launcher := launcherForTest(t, &recordingRunner{}, nil, "")
	_, err := launcher.Env(domainclient.LaunchSpec{Model: "exact", BaseURL: "http://127.0.0.1:8317", Token: "gm-1234567890123456"})
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.UnhandledInternal {
		t.Fatalf("Env error = %v, want UnhandledInternal", err)
	}
}

func TestSettingsWriteCreateIdempotentOverwrite(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	home := filepath.Join(t.TempDir(), "nested", "gemini-home")
	spec := domainclient.LaunchSpec{Client: domainclient.Gemini, Model: "exact", BaseURL: "http://127.0.0.1:8317", Token: "gm-1234567890123456", WorkingDir: workdir}

	launch := func(runner *recordingRunner) {
		t.Helper()
		launcher := launcherForTest(t, runner, func(context.Context, string) error { return nil }, home)
		if _, err := launcher.Launch(context.Background(), spec); err != nil {
			t.Fatalf("Launch error: %v", err)
		}
	}

	// Create: parent dirs are made private and settings.json appears with 0600.
	launch(&recordingRunner{output: []byte("0.52.0")})
	settingsPath := filepath.Join(home, settingsFileName)
	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("home mode = %o, want 700", dirInfo.Mode().Perm())
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != settingsJSON {
		t.Fatalf("settings = %q, want %q", body, settingsJSON)
	}

	// Idempotent: identical content is left byte-for-byte (and not rewritten).
	statBefore, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	launch(&recordingRunner{output: []byte("0.52.0")})
	statAfter, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if statBefore.ModTime() != statAfter.ModTime() {
		t.Fatal("identical settings were rewritten")
	}

	// Overwrite: drifted content is replaced with the PMux-owned document.
	if err := os.WriteFile(settingsPath, []byte(`{"drifted":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch(&recordingRunner{output: []byte("0.52.0")})
	body, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != settingsJSON {
		t.Fatalf("settings = %q after drift, want %q", body, settingsJSON)
	}
}

func TestLaunchReturnsClientExitResult(t *testing.T) {
	if os.Getenv("PMUX_GEMINI_EXIT_HELPER") == "1" {
		os.Exit(23)
	}
	t.Parallel()
	runner := &exitRunner{recordingRunner: recordingRunner{output: []byte("0.52.0")}}
	launcher := launcherForTest(t, runner, func(context.Context, string) error { return nil }, t.TempDir())
	result, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{Client: domainclient.Gemini, Model: "exact", BaseURL: "http://127.0.0.1:8317", Token: "gm-123456789012", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Launch error = %v, want client result", err)
	}
	if result.ExitCode != 23 || result.Signal != "" {
		t.Fatalf("Launch result = %+v, want exit code 23", result)
	}
}

func TestLaunchReturnsSignalResult(t *testing.T) {
	if os.Getenv("PMUX_GEMINI_SIGNAL_HELPER") == "1" {
		os.Exit(2)
	}
	if runtime.GOOS == "windows" {
		t.Skip("signal exit codes are unix-only")
	}
	t.Parallel()
	runner := &signalRunner{recordingRunner: recordingRunner{output: []byte("0.52.0")}}
	launcher := launcherForTest(t, runner, func(context.Context, string) error { return nil }, t.TempDir())
	result, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{Client: domainclient.Gemini, Model: "exact", BaseURL: "http://127.0.0.1:8317", Token: "gm-123456789012", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Launch error = %v, want client result", err)
	}
	if result.ExitCode != 128+9 || result.Signal == "" {
		t.Fatalf("Launch result = %+v, want SIGKILL result", result)
	}
}

type exitRunner struct{ recordingRunner }

func (r *exitRunner) Run(ctx context.Context, process Process) error {
	r.process = process
	r.runCalls++
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLaunchReturnsClientExitResult")
	cmd.Env = append(os.Environ(), "PMUX_GEMINI_EXIT_HELPER=1")
	return cmd.Run()
}

type signalRunner struct{ recordingRunner }

func (r *signalRunner) Run(ctx context.Context, process Process) error {
	r.process = process
	r.runCalls++
	cmd := exec.CommandContext(ctx, "sh", "-c", "kill -9 $$")
	return cmd.Run()
}

func TestPersistentSlotsUnsupported(t *testing.T) {
	t.Parallel()
	launcher := launcherForTest(t, &recordingRunner{}, nil, t.TempDir())
	if _, err := launcher.PlanPersist(context.Background(), domainclient.PersistSpec{}); err == nil || !strings.Contains(err.Error(), persistentSlotsMsg) {
		t.Fatalf("PlanPersist error = %v, want %q", err, persistentSlotsMsg)
	}
	if err := launcher.Upsert(context.Background(), domainclient.PersistPlan{}); err == nil || !strings.Contains(err.Error(), persistentSlotsMsg) {
		t.Fatalf("Upsert error = %v, want %q", err, persistentSlotsMsg)
	}
	if err := launcher.Unpersist(context.Background()); err == nil || !strings.Contains(err.Error(), persistentSlotsMsg) {
		t.Fatalf("Unpersist error = %v, want %q", err, persistentSlotsMsg)
	}
	var pmuxError *pmuxerr.Error
	if _, err := launcher.PlanPersist(context.Background(), domainclient.PersistSpec{}); !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.CodeUsage {
		t.Fatalf("PlanPersist error = %v, want CodeUsage", err)
	}
}
