package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func launcherForTest(t *testing.T, runner Runner) *Launcher {
	t.Helper()
	return New(Options{
		LookPath: func(name string) (string, error) {
			if name != "opencode" {
				t.Fatalf("looked up %q, want opencode", name)
			}
			return filepath.Join(t.TempDir(), "opencode"), nil
		},
		Runner:      runner,
		Environment: func() []string { return []string{"PATH=/bin", "KEEP=value"} },
	})
}

func TestClientID(t *testing.T) {
	t.Parallel()
	if got := New(Options{}).Client(); got != domainclient.OpenCode {
		t.Fatalf("Client() = %q, want %q", got, domainclient.OpenCode)
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
		{name: "bare version", output: "1.18.4\n", version: "1.18.4", supported: true},
		{name: "prerelease", output: "0.5.0-rc.1\n", version: "0.5.0", supported: true},
		{name: "v prefix", output: "v1.2.3\n", version: "1.2.3", supported: true},
		{name: "local dev build", output: "local\n", version: "local", supported: true},
		{name: "empty", output: "\n", version: "unknown", supported: false, wantError: true},
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
}

func TestEnvStripsConflictsAndBuildsConfig(t *testing.T) {
	t.Parallel()
	parent := []string{
		"PATH=/usr/bin", "KEEP=unchanged",
		"OPENCODE_CONFIG=/tmp/stale.json", "OPENCODE_CONFIG_CONTENT=old",
		"OPENCODE_CONFIG_DIR=/tmp/stale", "OPENCODE_DISABLE_PROJECT_CONFIG=0",
		"OPENCODE_PURE=1",
	}
	parentBefore := append([]string(nil), parent...)
	launcher := New(Options{Environment: func() []string { return parent }})
	spec := domainclient.LaunchSpec{
		Model: "exact-model", BaseURL: "http://127.0.0.1:8317", Token: "sk-canary-token",
	}
	env, err := launcher.Env(spec)
	if err != nil {
		t.Fatalf("Env error: %v", err)
	}
	if !reflect.DeepEqual(parent, parentBefore) {
		t.Fatalf("parent environment mutated: got %#v want %#v", parent, parentBefore)
	}
	values := envMap(env)
	want := map[string]string{
		"PATH": "/usr/bin", "KEEP": "unchanged",
		configContentEnv:        `{"$schema":"https://opencode.ai/config.json","model":"pmux/exact-model","provider":{"pmux":{"npm":"@ai-sdk/openai-compatible","name":"PMux (CLIProxyAPI)","options":{"baseURL":"http://127.0.0.1:8317/v1","apiKey":"sk-canary-token"},"models":{"exact-model":{"name":"exact-model"}}}}}`,
		disableProjectConfigEnv: "1",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("env = %#v, want %#v", values, want)
	}
}

func TestEnvConfigJSONShape(t *testing.T) {
	t.Parallel()
	launcher := New(Options{Environment: func() []string { return nil }})
	spec := domainclient.LaunchSpec{
		Model: "provider/model:thinking", BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456",
	}
	env, err := launcher.Env(spec)
	if err != nil {
		t.Fatalf("Env error: %v", err)
	}
	raw := envMap(env)[configContentEnv]
	var decoded struct {
		Schema   string `json:"$schema"`
		Model    string `json:"model"`
		Provider map[string]struct {
			NPM     string `json:"npm"`
			Name    string `json:"name"`
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("config content is not JSON: %v", err)
	}
	if decoded.Schema != "https://opencode.ai/config.json" {
		t.Fatalf("$schema = %q", decoded.Schema)
	}
	if decoded.Model != "pmux/provider/model:thinking" {
		t.Fatalf("model = %q, want pmux-prefixed exact model", decoded.Model)
	}
	pmux, ok := decoded.Provider["pmux"]
	if !ok || len(decoded.Provider) != 1 {
		t.Fatalf("provider keys = %#v, want only pmux", decoded.Provider)
	}
	if pmux.NPM != "@ai-sdk/openai-compatible" || pmux.Name != "PMux (CLIProxyAPI)" {
		t.Fatalf("provider identity = %q / %q", pmux.NPM, pmux.Name)
	}
	if pmux.Options.BaseURL != "http://127.0.0.1:8317/v1" {
		t.Fatalf("baseURL = %q, want /v1 suffix", pmux.Options.BaseURL)
	}
	if pmux.Options.APIKey != spec.Token {
		t.Fatalf("apiKey = %q, want launch token", pmux.Options.APIKey)
	}
	model, ok := pmux.Models[spec.Model]
	if !ok || len(pmux.Models) != 1 || model.Name != spec.Model {
		t.Fatalf("models = %#v, want only %q named identically", pmux.Models, spec.Model)
	}
}

func TestEnvRequiresBaseURLAndToken(t *testing.T) {
	t.Parallel()
	launcher := New(Options{Environment: func() []string { return nil }})
	for _, spec := range []domainclient.LaunchSpec{
		{Model: "m", Token: "tok"},
		{Model: "m", BaseURL: "http://127.0.0.1:8317"},
	} {
		_, err := launcher.Env(spec)
		var pmuxError *pmuxerr.Error
		if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.ConfigValidationFailed {
			t.Fatalf("Env(%+v) error = %v, want ConfigValidationFailed", spec, err)
		}
	}
}

func TestInvalidLaunchesNeverSpawnOpenCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		client  domainclient.ClientID
		model   string
		args    []string
		version string
	}{
		{name: "wrong client", client: domainclient.Claude, model: "exact-model", version: "1.18.4"},
		{name: "missing model", model: "", version: "1.18.4"},
		{name: "short model flag", model: "exact-model", args: []string{"-m", "other"}, version: "1.18.4"},
		{name: "long model flag", model: "exact-model", args: []string{"--model", "other"}, version: "1.18.4"},
		{name: "inline model flag", model: "exact-model", args: []string{"--model=other"}, version: "1.18.4"},
		{name: "empty version output", model: "exact-model", version: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte(test.version)}
			launcher := launcherForTest(t, runner)
			_, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
				Client: test.client, Model: test.model, Args: test.args,
				BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456", WorkingDir: t.TempDir(),
			})
			if err == nil {
				t.Fatal("Launch succeeded, want error")
			}
			if runner.runCalls != 0 {
				t.Fatalf("OpenCode spawn count = %d, want 0", runner.runCalls)
			}
		})
	}
}

func TestLaunchUsesExactArgvEnvironmentCWDAndJSONSeparation(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: []byte("1.18.4\n")}
	parent := []string{"PATH=/usr/bin", "KEEP=unchanged", "OPENCODE_CONFIG_CONTENT=old"}
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	stdin := strings.NewReader("input")
	workdir := t.TempDir()
	launcher := New(Options{
		LookPath:    func(string) (string, error) { return filepath.Join(workdir, "opencode"), nil },
		Runner:      runner,
		Environment: func() []string { return parent },
		Stdin:       stdin, Stdout: stdout, Stderr: stderr, JSONMode: true,
	})
	spec := domainclient.LaunchSpec{
		Client: domainclient.OpenCode, Model: "exact-model",
		BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456",
		Args: []string{"--session", "ses_123", "argument with spaces"}, WorkingDir: workdir,
	}
	result, err := launcher.Launch(context.Background(), spec)
	if err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	if result.ExitCode != 0 || result.Signal != "" {
		t.Fatalf("Launch result = %+v, want successful zero result", result)
	}
	// The model travels in OPENCODE_CONFIG_CONTENT only; argv is spec.Args verbatim.
	if !reflect.DeepEqual(runner.process.Args, spec.Args) {
		t.Fatalf("argv = %#v, want %#v", runner.process.Args, spec.Args)
	}
	if runner.process.Dir != workdir {
		t.Fatalf("cwd = %q, want %q", runner.process.Dir, workdir)
	}
	if runner.process.Stdin != stdin || runner.process.Stdout != stderr || runner.process.Stderr != stderr {
		t.Fatalf("stdio not routed for JSON separation: %#v", runner.process)
	}
	env := envMap(runner.process.Env)
	if env["PATH"] != "/usr/bin" || env["KEEP"] != "unchanged" || env[disableProjectConfigEnv] != "1" {
		t.Fatalf("child env = %#v", env)
	}
	config := env[configContentEnv]
	if config == "old" || !strings.Contains(config, `"apiKey":"`+spec.Token+`"`) || !strings.Contains(config, `"model":"pmux/exact-model"`) {
		t.Fatalf("config content = %q", config)
	}
	for _, arg := range runner.process.Args {
		if strings.Contains(arg, spec.Token) {
			t.Fatalf("token leaked into argv: %#v", runner.process.Args)
		}
	}
}

func TestLaunchReturnsClientExitResult(t *testing.T) {
	if os.Getenv("PMUX_OPENCODE_EXIT_HELPER") == "1" {
		os.Exit(23)
	}
	t.Parallel()
	runner := &exitRunner{recordingRunner: recordingRunner{output: []byte("1.18.4")}}
	launcher := launcherForTest(t, runner)
	result, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{
		Client: domainclient.OpenCode, Model: "exact",
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
	cmd.Env = append(os.Environ(), "PMUX_OPENCODE_EXIT_HELPER=1")
	return cmd.Run()
}

func TestPersistentSlotsAreClaudeOnly(t *testing.T) {
	t.Parallel()
	launcher := New(Options{})
	_, planErr := launcher.PlanPersist(context.Background(), domainclient.PersistSpec{})
	upsertErr := launcher.Upsert(context.Background(), domainclient.PersistPlan{})
	unpersistErr := launcher.Unpersist(context.Background())
	for _, err := range []error{planErr, upsertErr, unpersistErr} {
		var pmuxError *pmuxerr.Error
		if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.CodeUsage {
			t.Fatalf("error = %v, want typed usage error", err)
		}
		if pmuxError.Message != "Persistent model slots are supported only for the Claude client." {
			t.Fatalf("message = %q", pmuxError.Message)
		}
	}
}

func envMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, setting, _ := strings.Cut(value, "=")
		result[name] = setting
	}
	return result
}
