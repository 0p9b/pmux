package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

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

func launcherForTest(t *testing.T, runner Runner, preflight ModelPreflight, settings string) *Launcher {
	t.Helper()
	return New(Options{
		LookPath: func(name string) (string, error) {
			if name != "claude" {
				t.Fatalf("looked up %q, want claude", name)
			}
			return filepath.Join(t.TempDir(), "claude"), nil
		},
		Runner: runner,
		Environment: func() []string { return []string{"PATH=/bin", "KEEP=value"} },
		ModelPreflight: preflight,
		SettingsPath: settings,
		PersistenceStatePath: settings + ".state",
	})
}

func TestDetectVersionGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		output    string
		version   string
		supported bool
		wantError bool
	}{
		{name: "minimum", output: "2.0.0 (Claude Code)\n", version: "2.0.0", supported: true},
		{name: "newer with prefix", output: "Claude Code v2.17.4-beta.1\n", version: "2.17.4", supported: true},
		{name: "old", output: "1.99.9\n", version: "1.99.9", supported: false},
		{name: "unparseable", output: "Claude Code current\n", version: "unknown", supported: false, wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte(test.output)}
			launcher := launcherForTest(t, runner, nil, filepath.Join(t.TempDir(), "settings.json"))
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

func TestInvalidLaunchesNeverSpawnClaude(t *testing.T) {
	t.Parallel()
	unavailable := errors.New("model unavailable")
	cases := []struct {
		name      string
		version   string
		model     string
		preflight ModelPreflight
		want      error
	}{
		{name: "missing model", version: "2.1.0", model: ""},
		{name: "old client", version: "1.9.9", model: "exact-model", preflight: func(context.Context, string) error { return nil }},
		{name: "unparseable client", version: "current", model: "exact-model", preflight: func(context.Context, string) error { return nil }},
		{name: "model unavailable", version: "2.1.0", model: "missing-model", preflight: func(_ context.Context, model string) error {
			if model != "missing-model" { t.Fatalf("preflight model = %q", model) }
			return unavailable
		}, want: unavailable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{output: []byte(test.version)}
			launcher := launcherForTest(t, runner, test.preflight, filepath.Join(t.TempDir(), "settings.json"))
			_, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{Client: domainclient.Claude, Model: test.model, BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456", WorkingDir: t.TempDir()})
			if err == nil {
				t.Fatal("Launch succeeded, want error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Launch error = %v, want %v", err, test.want)
			}
			if runner.runCalls != 0 {
				t.Fatalf("Claude spawn count = %d, want 0", runner.runCalls)
			}
		})
	}
}

func TestLaunchUsesExactArgvEnvironmentCWDAndJSONSeparation(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: []byte("2.1.215\n")}
	parent := []string{
		"PATH=/usr/bin", "KEEP=unchanged", "ANTHROPIC_BASE_URL=https://wrong.invalid",
		"ANTHROPIC_AUTH_TOKEN=old", "ANTHROPIC_API_KEY=old-api", "ANTHROPIC_MODEL=wrong",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=wrong-opus",
	}
	parentBefore := append([]string(nil), parent...)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	stdin := strings.NewReader("input")
	workdir := t.TempDir()
	var preflightModel string
	launcher := New(Options{
		LookPath: func(string) (string, error) { return filepath.Join(workdir, "claude"), nil },
		Runner: runner,
		Environment: func() []string { return parent },
		ModelPreflight: func(_ context.Context, model string) error { preflightModel = model; return nil },
		Stdin: stdin, Stdout: stdout, Stderr: stderr, JSONMode: true,
		SettingsPath: filepath.Join(t.TempDir(), "settings.json"),
	})
	spec := domainclient.LaunchSpec{
		Client: domainclient.Claude, Model: "provider/model:thinking(32k)",
		BaseURL: "http://127.0.0.1:8317", Token: "sk-1234567890123456",
		Args: []string{"--permission-mode", "plan", "argument with spaces"}, WorkingDir: workdir,
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
	wantArgs := []string{"--model", spec.Model, "--permission-mode", "plan", "argument with spaces"}
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
	wantEnv := map[string]string{"PATH": "/usr/bin", "KEEP": "unchanged", baseURLEnv: spec.BaseURL, authTokenEnv: spec.Token}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("child env = %#v, want %#v", env, wantEnv)
	}
}

func TestNormalLaunchDoesNotMutateSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	original := []byte("{\n  \"theme\" : \"dark\",\n  \"env\": {\"UNRELATED\":\"yes\"}\n}\n")
	if err := os.WriteFile(settings, original, 0o600); err != nil { t.Fatal(err) }
	runner := &recordingRunner{output: []byte("2.2.0")}
	launcher := launcherForTest(t, runner, func(context.Context, string) error { return nil }, settings)
	if _, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{Client: domainclient.Claude, Model: "exact", BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012", WorkingDir: dir}); err != nil {
		t.Fatalf("Launch error: %v", err)
	}
	after, err := os.ReadFile(settings)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(after, original) {
		t.Fatalf("normal launch changed settings\n got: %q\nwant: %q", after, original)
	}
	if _, err := os.Stat(settings + ".state"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal launch created persistence state: %v", err)
	}
}

func TestLaunchReturnsClientExitResult(t *testing.T) {
	if os.Getenv("PMUX_CLAUDE_EXIT_HELPER") == "1" {
		os.Exit(23)
	}
	t.Parallel()
	runner := &exitRunner{recordingRunner: recordingRunner{output: []byte("2.1.0")}}
	launcher := launcherForTest(t, runner, func(context.Context, string) error { return nil }, filepath.Join(t.TempDir(), "settings.json"))
	result, err := launcher.Launch(context.Background(), domainclient.LaunchSpec{Client: domainclient.Claude, Model: "exact", BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Launch error = %v, want client result", err)
	}
	if result.ExitCode != 23 || result.Signal != "" {
		t.Fatalf("Launch result = %+v, want exit code 23", result)
	}
}

type exitRunner struct { recordingRunner }

func (r *exitRunner) Run(ctx context.Context, process Process) error {
	r.process = process
	r.runCalls++
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLaunchReturnsClientExitResult")
	cmd.Env = append(os.Environ(), "PMUX_CLAUDE_EXIT_HELPER=1")
	return cmd.Run()
}

func TestPersistentSlotsRestoreSettingsByteForByte(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	state := filepath.Join(dir, "pmux-state", "claude.json")
	original := []byte("{\n  \"theme\" : \"dark\",\n  \"env\": { \"UNRELATED\" : \"kept\", \"ANTHROPIC_AUTH_TOKEN\": \"old-secret-canary\", \"ANTHROPIC_DEFAULT_HAIKU_MODEL\": \"old-haiku\" },\n  \"permissions\": {\"allow\": [\"Read\"]}\n}\n")
	if err := os.WriteFile(settings, original, 0o644); err != nil { t.Fatal(err) }
	var preflighted []string
	secret := "sk-canary-1234567890"
	launcher := New(Options{
		SettingsPath: settings, PersistenceStatePath: state,
		ModelPreflight: func(_ context.Context, model string) error { preflighted = append(preflighted, model); return nil },
		Now: func() time.Time { return time.Date(2026, 7, 20, 12, 34, 56, 0, time.UTC) },
	})
	plan, err := launcher.PlanPersistent(context.Background(), PersistentSpec{
		BaseURL: "http://127.0.0.1:8317", Token: secret,
		Slots: PersistentSlots{
			Opus: SlotUpdate{Action: SlotSet, Model: "exact-opus"},
			Sonnet: SlotUpdate{Action: SlotSet, Model: "exact-sonnet"},
			Haiku: SlotUpdate{Action: SlotUnmanaged},
		},
	})
	if err != nil { t.Fatalf("PlanPersistent error: %v", err) }
	if !reflect.DeepEqual(preflighted, []string{"exact-opus", "exact-sonnet"}) {
		t.Fatalf("preflighted = %#v", preflighted)
	}
	if strings.Contains(plan.Diff, secret) {
		t.Fatal("persistence diff contains complete secret")
	}
	if strings.Contains(plan.Diff, "old-secret-canary") {
		t.Fatal("persistence diff contains a pre-existing complete secret")
	}
	if !strings.Contains(plan.Diff, "sk-cana…7890") {
		t.Fatalf("persistence diff does not contain expected mask: %s", plan.Diff)
	}
	if err := launcher.Upsert(context.Background(), plan); err != nil {
		t.Fatalf("Persist error: %v", err)
	}
	persisted, err := os.ReadFile(settings)
	if err != nil { t.Fatal(err) }
	var decoded struct { Env map[string]string `json:"env"`; Theme string `json:"theme"`; Permissions map[string][]string `json:"permissions"` }
	if err := jsonUnmarshal(persisted, &decoded); err != nil { t.Fatal(err) }
	if decoded.Env[baseURLEnv] != "http://127.0.0.1:8317" || decoded.Env[authTokenEnv] != secret || decoded.Env[opusEnv] != "exact-opus" || decoded.Env[sonnetEnv] != "exact-sonnet" {
		t.Fatalf("persisted env = %#v", decoded.Env)
	}
	if _, found := decoded.Env[haikuEnv]; found {
		t.Fatalf("unmanaged haiku slot remained: %#v", decoded.Env)
	}
	if decoded.Theme != "dark" || !reflect.DeepEqual(decoded.Permissions["allow"], []string{"Read"}) || decoded.Env["UNRELATED"] != "kept" {
		t.Fatalf("unrelated settings changed semantically: %+v", decoded)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(settings)
		if err != nil { t.Fatal(err) }
		if info.Mode().Perm() != 0o600 { t.Fatalf("settings mode = %o, want 600", info.Mode().Perm()) }
	}
	backups, err := filepath.Glob(settings + ".pmux.*.bak")
	if err != nil || len(backups) != 1 { t.Fatalf("backups = %#v, err = %v", backups, err) }
	backup, err := os.ReadFile(backups[0])
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(backup, original) { t.Fatal("backup is not byte-identical to original") }
	if err := launcher.Unpersist(context.Background()); err != nil {
		t.Fatalf("Unpersist error: %v", err)
	}
	restored, err := os.ReadFile(settings)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(restored, original) {
		t.Fatalf("restored settings differ\n got: %q\nwant: %q", restored, original)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) { t.Fatalf("state remains: %v", err) }
	if _, err := os.Stat(backups[0]); !errors.Is(err, os.ErrNotExist) { t.Fatalf("backup remains: %v", err) }
}

func TestPersistentSlotUpsertRetainsOriginalBackupAndOtherSlots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	statePath := filepath.Join(dir, "state.json")
	original := []byte("{\"theme\":\"dark\",\"env\":{\"UNRELATED\":\"kept\"}}\n")
	if err := os.WriteFile(settings, original, 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := New(Options{
		SettingsPath: settings, PersistenceStatePath: statePath,
		ModelPreflight: func(context.Context, string) error { return nil },
	})
	first, err := launcher.PlanPersist(context.Background(), domainclient.PersistSpec{
		BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012",
		Slots: domainclient.PersistentSlots{Opus: domainclient.SlotUpdate{Action: domainclient.SlotSet, Model: "opus-live"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Upsert(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second, err := launcher.PlanPersist(context.Background(), domainclient.PersistSpec{
		BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012",
		Slots: domainclient.PersistentSlots{
			Opus: domainclient.SlotUpdate{Action: domainclient.SlotUnmanaged},
			Sonnet: domainclient.SlotUpdate{Action: domainclient.SlotSet, Model: "sonnet-live"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Upsert(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	var settingsValue struct{ Env map[string]string `json:"env"` }
	body, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal(body, &settingsValue); err != nil {
		t.Fatal(err)
	}
	if _, found := settingsValue.Env[opusEnv]; found || settingsValue.Env[sonnetEnv] != "sonnet-live" || settingsValue.Env["UNRELATED"] != "kept" {
		t.Fatalf("upserted env=%#v", settingsValue.Env)
	}
	backups, err := filepath.Glob(settings + ".pmux.*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%#v err=%v", backups, err)
	}
	if err := launcher.Unpersist(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(settings)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("restored=%q err=%v", restored, err)
	}
}

func TestUnpersistRefusesConcurrentSettingsChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	original := []byte("{\"theme\":\"dark\"}\n")
	if err := os.WriteFile(settings, original, 0o600); err != nil { t.Fatal(err) }
	launcher := New(Options{SettingsPath: settings, PersistenceStatePath: filepath.Join(dir, "state.json")})
	plan, err := launcher.PlanPersistent(context.Background(), PersistentSpec{BaseURL: "http://127.0.0.1:8317", Token: "sk-123456789012"})
	if err != nil { t.Fatal(err) }
	if err := launcher.Upsert(context.Background(), plan); err != nil { t.Fatal(err) }
	external := []byte("{\"user_changed\":true}\n")
	if err := os.WriteFile(settings, external, 0o600); err != nil { t.Fatal(err) }
	err = launcher.Unpersist(context.Background())
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.ClientSettingsConflict {
		t.Fatalf("Unpersist error = %#v, want %s", err, pmuxerr.ClientSettingsConflict)
	}
	got, readErr := os.ReadFile(settings)
	if readErr != nil { t.Fatal(readErr) }
	if !bytes.Equal(got, external) { t.Fatal("Unpersist overwrote concurrent user change") }
}

func envMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) == 2 { result[parts[0]] = parts[1] }
	}
	return result
}

// A tiny indirection keeps the test's JSON use local without changing the
// production adapter's public surface.
func jsonUnmarshal(body []byte, value any) error {
	return json.Unmarshal(body, value)
}
