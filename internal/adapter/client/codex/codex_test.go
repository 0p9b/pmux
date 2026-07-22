package codex

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
	"syscall"
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

func launcherForTest(t *testing.T, runner Runner) *Launcher {
	t.Helper()
	return New(Options{
		LookPath: func(name string) (string, error) {
			if name != "codex" {
				t.Fatalf("looked up %q, want codex", name)
			}
			return filepath.Join(t.TempDir(), "codex"), nil
		},
		Runner:      runner,
		Environment: func() []string { return []string{"PATH=/bin", "KEEP=value"} },
	})
}

func TestClientID(t *testing.T) {
	t.Parallel()
	if got := New(Options{}).Client(); got != domainclient.Codex {
		t.Fatalf("Client() = %q, want %q", got, domainclient.Codex)
	}
}

func TestDetectVersionParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		output    string
		version   string
		supported bool
		wantError bool
	}{
		{name: "plain", output: "codex-cli 0.20.0\n", version: "0.20.0", supported: true},
		{name: "v prefix", output: "codex-cli v1.2.3\n", version: "1.2.3", supported: true},
		{name: "trailing noise", output: "codex-cli 0.5.2 (build abc123)\n", version: "0.5.2", supported: true},
		{name: "unparseable", output: "codex version 1.2.3\n", version: "unknown", supported: false, wantError: true},
		{name: "empty", output: "", version: "unknown", supported: false, wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte(test.output)}
			launcher := launcherForTest(t, runner)
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
	if !strings.Contains(pmuxError.Message, "Install Codex CLI") {
		t.Fatalf("error message = %q, want install guidance", pmuxError.Message)
	}
}

func TestEnvStripsConflictsAndInjectsToken(t *testing.T) {
	t.Parallel()
	parent := []string{
		"PATH=/usr/bin", "KEEP=unchanged", "OPENAI_API_KEY=old-key",
		"CODEX_API_KEY=old-codex", "OPENAI_BASE_URL=https://wrong.invalid",
		"CODEX_HOME=/tmp/wrong", "CODEX_MODEL=wrong-model",
	}
	parentBefore := append([]string(nil), parent...)
	launcher := New(Options{Environment: func() []string { return parent }})
	env, err := launcher.Env(domainclient.LaunchSpec{BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456"})
	if err != nil {
		t.Fatalf("Env error: %v", err)
	}
	if !reflect.DeepEqual(parent, parentBefore) {
		t.Fatalf("parent environment mutated: got %#v want %#v", parent, parentBefore)
	}
	got := envMap(env)
	want := map[string]string{"PATH": "/usr/bin", "KEEP": "unchanged", apiKeyEnv: "sk-1234567890123456"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %#v, want %#v", got, want)
	}
}

func TestEnvRequiresBaseURLAndToken(t *testing.T) {
	t.Parallel()
	launcher := launcherForTest(t, &recordingRunner{})
	cases := []struct {
		name    string
		baseURL string
		token   string
	}{
		{name: "missing base URL", baseURL: "", token: "sk-1234567890123456"},
		{name: "missing token", baseURL: "http://127.0.0.1:8317", token: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := launcher.Env(domainclient.LaunchSpec{BaseURL: test.baseURL, Token: test.token})
			var pmuxError *pmuxerr.Error
			if !errors.As(err, &pmuxError) {
				t.Fatalf("Env error = %T %v, want *pmuxerr.Error", err, err)
			}
			if pmuxError.Code != pmuxerr.ConfigValidationFailed || pmuxError.Class != pmuxerr.User {
				t.Fatalf("Env error = %#v, want %s/%s", pmuxError, pmuxerr.ConfigValidationFailed, pmuxerr.User)
			}
		})
	}
}

func TestLaunchUsesExactArgvEnvironmentAndCWD(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: []byte("codex-cli 0.20.0\n")}
	parent := []string{"PATH=/usr/bin", "KEEP=unchanged", "OPENAI_API_KEY=old", "CODEX_HOME=/wrong"}
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	stdin := strings.NewReader("input")
	workdir := t.TempDir()
	launcher := New(Options{
		LookPath:    func(string) (string, error) { return filepath.Join(workdir, "codex"), nil },
		Runner:      runner,
		Environment: func() []string { return parent },
		Stdin:       stdin, Stdout: stdout, Stderr: stderr,
	})
	spec := domainclient.LaunchSpec{
		Client: domainclient.Codex, Model: "gpt-5.2-codex",
		BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456",
		Args: []string{"--full-auto", "argument with spaces"}, WorkingDir: workdir,
	}
	result, err := launcher.Launch(context.Background(), spec)
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	if result.ExitCode != 0 || result.Signal != "" {
		t.Fatalf("Launch result = %+v, want successful zero result", result)
	}
	wantArgs := []string{
		"-m", "gpt-5.2-codex",
		"-c", `model_providers.pmux={ name = "pmux", base_url = "http://127.0.0.1:8317/v1", env_key = "OPENAI_API_KEY", wire_api = "responses" }`,
		"-c", `model_provider="pmux"`,
		"--full-auto", "argument with spaces",
	}
	if !reflect.DeepEqual(runner.process.Args, wantArgs) {
		t.Fatalf("argv = %#v, want %#v", runner.process.Args, wantArgs)
	}
	if runner.process.Dir != workdir {
		t.Fatalf("cwd = %q, want %q", runner.process.Dir, workdir)
	}
	if runner.process.Stdin != stdin || runner.process.Stdout != stdout || runner.process.Stderr != stderr {
		t.Fatalf("stdio not routed: %#v", runner.process)
	}
	env := envMap(runner.process.Env)
	wantEnv := map[string]string{"PATH": "/usr/bin", "KEEP": "unchanged", apiKeyEnv: spec.Token}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("child env = %#v, want %#v", env, wantEnv)
	}
}

func TestLaunchRejectsClientManagedArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{name: "short model", args: []string{"-m", "other"}},
		{name: "long model", args: []string{"--model", "other"}},
		{name: "inline model", args: []string{"--model=other"}},
		{name: "short config", args: []string{"-c", `model_provider="x"`}},
		{name: "long config", args: []string{"--config", `model_provider="x"`}},
		{name: "inline config", args: []string{"--config=model_provider=\"x\""}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte("codex-cli 0.20.0\n")}
			launcher := launcherForTest(t, runner)
			_, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
				Client: domainclient.Codex, Model: "exact-model",
				BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456",
				Args: test.args, WorkingDir: t.TempDir(),
			})
			var pmuxError *pmuxerr.Error
			if !errors.As(err, &pmuxError) {
				t.Fatalf("Launch error = %T %v, want *pmuxerr.Error", err, err)
			}
			if pmuxError.Code != pmuxerr.CodeUsage || pmuxError.Class != pmuxerr.User {
				t.Fatalf("Launch error = %#v, want %s/%s", pmuxError, pmuxerr.CodeUsage, pmuxerr.User)
			}
			if runner.runCalls != 0 {
				t.Fatalf("Codex spawn count = %d, want 0", runner.runCalls)
			}
		})
	}
}

func TestLaunchRejectsUnsupportedClient(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: []byte("codex-cli 0.20.0\n")}
	launcher := launcherForTest(t, runner)
	_, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
		Client: domainclient.Claude, Model: "exact-model",
		BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456",
		WorkingDir: t.TempDir(),
	})
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) {
		t.Fatalf("Launch error = %T %v, want *pmuxerr.Error", err, err)
	}
	if pmuxError.Code != pmuxerr.CodeUsage {
		t.Fatalf("error code = %q, want %q", pmuxError.Code, pmuxerr.CodeUsage)
	}
	if runner.runCalls != 0 {
		t.Fatalf("Codex spawn count = %d, want 0", runner.runCalls)
	}
}

func TestInvalidLaunchesNeverSpawnCodex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		version string
		model   string
		baseURL string
		token   string
	}{
		{name: "missing model", version: "codex-cli 0.20.0", model: "", baseURL: "http://127.0.0.1:8317", token: "sk-1234567890123456"},
		{name: "unparseable client", version: "current", model: "exact-model", baseURL: "http://127.0.0.1:8317", token: "sk-1234567890123456"},
		{name: "missing base URL", version: "codex-cli 0.20.0", model: "exact-model", baseURL: "", token: "sk-1234567890123456"},
		{name: "missing token", version: "codex-cli 0.20.0", model: "exact-model", baseURL: "http://127.0.0.1:8317", token: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte(test.version)}
			launcher := launcherForTest(t, runner)
			_, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
				Client: domainclient.Codex, Model: test.model,
				BaseURL: test.baseURL, Token: test.token, WorkingDir: t.TempDir(),
			})
			if err == nil {
				t.Fatal("Launch succeeded, want error")
			}
			if runner.runCalls != 0 {
				t.Fatalf("Codex spawn count = %d, want 0", runner.runCalls)
			}
		})
	}
}

func TestLaunchReturnsClientExitResult(t *testing.T) {
	if os.Getenv("PMUX_CODEX_EXIT_HELPER") == "1" {
		os.Exit(23)
	}
	t.Parallel()
	runner := &exitRunner{recordingRunner: recordingRunner{output: []byte("codex-cli 0.20.0\n")}}
	launcher := launcherForTest(t, runner)
	result, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
		Client: domainclient.Codex, Model: "exact",
		BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012", WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Launch error = %v, want client result", err)
	}
	if result.ExitCode != 23 || result.Signal != "" {
		t.Fatalf("Launch result = %+v, want exit code 23", result)
	}
}

type exitRunner struct{ recordingRunner }

func (r *exitRunner) Run(ctx context.Context, process Process) error {
	r.process = process
	r.runCalls++
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLaunchReturnsClientExitResult")
	cmd.Env = append(os.Environ(), "PMUX_CODEX_EXIT_HELPER=1")
	return cmd.Run()
}

func TestLaunchReturnsSignalResult(t *testing.T) {
	if os.Getenv("PMUX_CODEX_SIGNAL_HELPER") == "1" {
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			os.Exit(1)
		}
		if err := process.Signal(syscall.SIGTERM); err != nil {
			os.Exit(1)
		}
		select {}
	}
	if runtime.GOOS == "windows" {
		t.Skip("signal exit mapping requires a unix process state")
	}
	t.Parallel()
	runner := &signalRunner{recordingRunner: recordingRunner{output: []byte("codex-cli 0.20.0\n")}}
	launcher := launcherForTest(t, runner)
	result, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
		Client: domainclient.Codex, Model: "exact",
		BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012", WorkingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Launch error = %v, want client result", err)
	}
	if result.ExitCode != 128+int(syscall.SIGTERM) || result.Signal != syscall.SIGTERM.String() {
		t.Fatalf("Launch result = %+v, want exit code %d signal %q", result, 128+int(syscall.SIGTERM), syscall.SIGTERM)
	}
}

type signalRunner struct{ recordingRunner }

func (r *signalRunner) Run(ctx context.Context, process Process) error {
	r.process = process
	r.runCalls++
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLaunchReturnsSignalResult")
	cmd.Env = append(os.Environ(), "PMUX_CODEX_SIGNAL_HELPER=1")
	return cmd.Run()
}

func TestPersistentSlotsUnsupported(t *testing.T) {
	t.Parallel()
	launcher := launcherForTest(t, &recordingRunner{output: []byte("codex-cli 0.20.0\n")})
	plan, err := launcher.PlanPersist(context.Background(), domainclient.PersistSpec{
		BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012",
	})
	assertPersistUnsupported(t, err)
	if !reflect.DeepEqual(plan, domainclient.PersistPlan{}) {
		t.Fatalf("PlanPersist plan = %+v, want zero value", plan)
	}
	if err := launcher.Upsert(context.Background(), domainclient.PersistPlan{}); true {
		assertPersistUnsupported(t, err)
	}
	assertPersistUnsupported(t, launcher.Unpersist(context.Background()))
}

func assertPersistUnsupported(t *testing.T, err error) {
	t.Helper()
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) {
		t.Fatalf("error = %T %v, want *pmuxerr.Error", err, err)
	}
	if pmuxError.Code != pmuxerr.CodeUsage || pmuxError.Class != pmuxerr.User {
		t.Fatalf("error = %#v, want %s/%s", pmuxError, pmuxerr.CodeUsage, pmuxerr.User)
	}
	if pmuxError.Message != "Persistent model slots are supported only for the Claude client." {
		t.Fatalf("error message = %q", pmuxError.Message)
	}
}

func envMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}
