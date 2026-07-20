package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/version"
	"github.com/spf13/cobra"
)

type commandSpy struct {
	calls  []app.Invocation
	result app.Result
	err    error
}

func (s *commandSpy) Execute(_ context.Context, invocation app.Invocation, sink app.EventSink) (app.Result, error) {
	s.calls = append(s.calls, invocation)
	return s.result, s.err
}

func testDependencies(spy app.UseCases, terminal bool, out, stderr *bytes.Buffer) dependencies {
	return dependencies{
		UseCases: spy, Out: out, Err: stderr, In: strings.NewReader(""),
		IsTerminal: func() bool { return terminal },
		Version: func() version.Info {
			return version.Info{Version: "1.2.3", Commit: "abc123", Date: "2026-07-20T00:00:00Z"}
		},
		GOOS: "linux", GOARCH: "amd64", Getenv: func(string) string { return "" },
		UserHome: func() (string, error) { return "/home/tester", nil },
	}
}

func TestCanonicalCommandTree(t *testing.T) {
	root := newRootCommand(testDependencies(&commandSpy{}, false, &bytes.Buffer{}, &bytes.Buffer{}))
	assertChildren(t, root, []string{"claude", "completion", "config", "doctor", "launch", "models", "providers", "service", "setup", "update", "version"})
	assertChildren(t, child(t, root, "providers"), []string{"disable", "enable", "list", "login", "remove", "verify"})
	assertChildren(t, child(t, root, "models"), []string{"favorite", "list", "test", "unfavorite"})
	assertChildren(t, child(t, root, "service"), []string{"install", "logs", "restart", "start", "status", "stop", "uninstall"})
	assertChildren(t, child(t, root, "config"), []string{"backup", "edit", "get", "restore", "set", "show"})
	assertChildren(t, child(t, root, "update"), []string{"check", "proxy", "self"})

	for _, forbidden := range []string{"adopt", "auth", "env", "fleet", "install", "keys", "status", "use"} {
		if findChild(root, forbidden) != nil {
			t.Fatalf("forbidden top-level command %q is registered", forbidden)
		}
	}
	walk(root, func(cmd *cobra.Command) {
		if len(cmd.Aliases) != 0 {
			t.Errorf("%s has unapproved aliases %v", cmd.CommandPath(), cmd.Aliases)
		}
	})
}

func TestGlobalJSONUsesFiniteEnvelopeAndForbidsInteractiveMode(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{result: app.Result{Data: map[string]any{"providers": []any{}}}}
	code := execute(context.Background(), testDependencies(spy, true, out, stderr), []string{"--json", "providers"})
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if len(spy.calls) != 1 {
		t.Fatalf("calls = %d", len(spy.calls))
	}
	got := spy.calls[0]
	if got.Operation != "providers.list" {
		t.Errorf("operation = %q", got.Operation)
	}
	if got.Interactive {
		t.Error("JSON invocation allowed prompts/TUI")
	}
	if !got.JSON {
		t.Error("JSON flag not propagated")
	}
	var envelope struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if !envelope.OK {
		t.Error("success envelope ok=false")
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestClientExitStatusRendersResultAndPassesThrough(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
			out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			spy := &commandSpy{result: app.Result{
				Data:     map[string]any{"client": "claude", "origin": "client", "exit_code": 42},
				Human:    []string{"Claude Code exited with status 42."},
				ExitCode: 42,
			}}
			args := []string{"launch", "--client", "claude", "--model", "live-model"}
			if jsonMode {
				args = append([]string{"--json"}, args...)
			}
			code := execute(context.Background(), testDependencies(spy, false, out, stderr), args)
			if code != 42 {
				t.Fatalf("exit = %d, want client exit 42; stdout=%s stderr=%s", code, out, stderr)
			}
			if stderr.Len() != 0 {
				t.Fatalf("client exit rendered as PMux error: %s", stderr)
			}
			if jsonMode {
				var envelope struct {
					OK   bool `json:"ok"`
					Data struct {
						Origin   string `json:"origin"`
						ExitCode int    `json:"exit_code"`
					} `json:"data"`
				}
				if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
					t.Fatalf("invalid JSON %q: %v", out, err)
				}
				if !envelope.OK || envelope.Data.Origin != "client" || envelope.Data.ExitCode != 42 {
					t.Fatalf("envelope = %+v", envelope)
				}
			} else if !strings.Contains(out.String(), "Claude Code exited with status 42.") {
				t.Fatalf("human output = %q", out)
			}
		})
	}
}
func TestForegroundAttachmentWaitsAfterStartupResultIsRendered(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	waited := false
	spy := &commandSpy{result: app.Result{
		Data: map[string]string{"state": "running"}, Human: []string{"Service is running."},
		Attachment: func(context.Context) error {
			waited = true
			if !strings.Contains(out.String(), "Service is running.") {
				t.Fatal("attachment waited before startup result was rendered")
			}
			return nil
		},
	}}
	code := execute(context.Background(), testDependencies(spy, true, out, stderr), []string{"service", "start", "--foreground"})
	if code != 0 || !waited {
		t.Fatalf("exit=%d waited=%t stderr=%s", code, waited, stderr)
	}
}
func TestJSONForegroundStartIsRejectedBeforeDispatch(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{}
	code := execute(context.Background(), testDependencies(spy, true, out, stderr), []string{"--json", "service", "start", "--foreground"})
	if code != 2 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, stderr)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("foreground start dispatched under JSON: %#v", spy.calls)
	}
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if envelope.OK {
		t.Fatalf("JSON foreground rejection reported success: %s", out)
	}
}

func TestCanonicalFlags(t *testing.T) {
	root := newRootCommand(testDependencies(&commandSpy{}, false, &bytes.Buffer{}, &bytes.Buffer{}))
	assertFlags(t, root, true, []string{"config-dir", "json", "verbose", "yes"})
	assertFlags(t, child(t, root, "config"), true, []string{"scope"})
	cases := map[string][]string{
		"setup":            {"config-path", "harden", "mode", "proxy-path"},
		"providers/list":   {"enabled", "refresh", "status", "type"},
		"providers/login":  {"api-key-file", "api-key-stdin", "callback-url-stdin", "method", "no-browser", "service-account", "vertex-prefix"},
		"providers/verify": {"account", "refresh-models"},
		"providers/remove": {"keep-credentials"},
		"models/list":      {"available", "favorite", "provider", "refresh", "search"},
		"models/test":      {"provider", "timeout"},
		"launch":           {"client", "model"},
		"doctor":           {"bundle", "check", "fix", "online"},
		"service/start":    {"foreground"},
		"service/stop":     {"timeout"},
		"service/restart":  {"timeout"},
		"service/install":  {"start"},
		"service/logs":     {"clear", "follow", "level", "lines", "output", "since", "source"},
		"config/show":      {"effective", "reveal-paths"},
		"config/set":       {"restart"},
		"config/edit":      {"editor", "restart"},
		"config/restore":   {"restart"},
		"update/check":     {"component"},
		"update/self":      {"version"},
		"update/proxy":     {"version"},
		"version":          {"short"},
	}
	for path, expected := range cases {
		command := root
		for _, part := range strings.Split(path, "/") {
			command = child(t, command, part)
		}
		assertFlags(t, command, false, expected)
	}
}

func TestJSONLoginNeverAllowsPrompting(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{result: app.Result{Data: map[string]string{"status": "started"}}}
	code := execute(context.Background(), testDependencies(spy, true, out, stderr), []string{"--json", "providers", "login", "codex", "--method", "device", "--no-browser"})
	if code != 0 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, out, stderr)
	}
	if len(spy.calls) != 1 || spy.calls[0].Interactive {
		t.Fatalf("JSON login invocation = %#v", spy.calls)
	}
}

func TestBareParentBehavior(t *testing.T) {
	cases := []struct {
		args      []string
		terminal  bool
		operation string
	}{
		{nil, true, "tui.dashboard"}, {nil, false, "dashboard.status"},
		{[]string{"providers"}, true, "tui.providers"}, {[]string{"providers"}, false, "providers.list"},
		{[]string{"models"}, true, "tui.models"}, {[]string{"config"}, true, "tui.config"},
		{[]string{"service"}, true, "tui.service"}, {[]string{"update"}, true, "update.check"},
	}
	for _, tc := range cases {
		t.Run(tc.operation, func(t *testing.T) {
			spy := &commandSpy{result: app.Result{Data: map[string]any{}}}
			code := execute(context.Background(), testDependencies(spy, tc.terminal, &bytes.Buffer{}, &bytes.Buffer{}), tc.args)
			if code != 0 {
				t.Fatalf("exit=%d", code)
			}
			if len(spy.calls) != 1 || spy.calls[0].Operation != app.Operation(tc.operation) {
				t.Fatalf("calls=%#v", spy.calls)
			}
		})
	}
}
func TestBareNonTTYParentsMatchExplicitReadCommands(t *testing.T) {
	pairs := []struct{ bare, explicit []string }{
		{[]string{"providers"}, []string{"providers", "list"}},
		{[]string{"models"}, []string{"models", "list"}},
		{[]string{"config"}, []string{"config", "show"}},
		{[]string{"service"}, []string{"service", "status"}},
		{[]string{"update"}, []string{"update", "check"}},
	}
	for _, pair := range pairs {
		var got []app.Invocation
		for _, args := range [][]string{pair.bare, pair.explicit} {
			spy := &commandSpy{result: app.Result{Data: map[string]any{}}}
			if code := execute(context.Background(), testDependencies(spy, false, &bytes.Buffer{}, &bytes.Buffer{}), args); code != 0 {
				t.Fatalf("%v exit=%d", args, code)
			}
			got = append(got, spy.calls[0])
		}
		if !reflect.DeepEqual(got[0], got[1]) {
			t.Errorf("%v differs from %v: %#v != %#v", pair.bare, pair.explicit, got[0], got[1])
		}
	}
}

func TestVersionReportingPaths(t *testing.T) {
	t.Run("short", func(t *testing.T) {
		out := &bytes.Buffer{}
		spy := &commandSpy{err: errors.New("version must not call application services")}
		code := execute(context.Background(), testDependencies(spy, false, out, &bytes.Buffer{}), []string{"version", "--short"})
		if code != 0 || out.String() != "1.2.3\n" {
			t.Fatalf("exit=%d output=%q", code, out.String())
		}
		if len(spy.calls) != 0 {
			t.Fatal("version called application use cases")
		}
	})
	t.Run("human", func(t *testing.T) {
		out := &bytes.Buffer{}
		code := execute(context.Background(), testDependencies(&commandSpy{}, false, out, &bytes.Buffer{}), []string{"version"})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		for _, want := range []string{"PMux 1.2.3", "Platform: linux/amd64", "Config root: " + filepath.Join("/home/tester", ".config", "pmux"), "CLIProxyAPI: unknown", "Commit: abc123"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("version output missing %q: %s", want, out)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		out := &bytes.Buffer{}
		code := execute(context.Background(), testDependencies(&commandSpy{}, false, out, &bytes.Buffer{}), []string{"--json", "version"})
		if code != 0 {
			t.Fatalf("exit=%d output=%s", code, out)
		}
		var envelope struct {
			OK   bool          `json:"ok"`
			Data versionReport `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if !envelope.OK || envelope.Data.PMuxVersion != "1.2.3" || envelope.Data.CLIProxyAPI != "unknown" {
			t.Fatalf("envelope=%+v", envelope)
		}
	})
}

func TestConfigDirIsAbsoluteAndPropagated(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{result: app.Result{Data: map[string]any{}}}
	code := execute(context.Background(), testDependencies(spy, false, out, stderr), []string{"--config-dir", "relative/config", "providers", "list"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if len(spy.calls) != 1 || !filepath.IsAbs(spy.calls[0].ConfigDir) {
		t.Fatalf("config dir not absolute: %#v", spy.calls)
	}
}

func TestCanonicalExitMappingAndJSONErrorEnvelope(t *testing.T) {
	cases := []struct {
		name string
		err  error
		exit int
	}{
		{"safe mode condition", pmuxerr.New(pmuxerr.ConfigSafeMode, pmuxerr.Environment, "safe mode"), 7},
		{"usage outcome", pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "bad input"), 2},
		{"authentication outcome", pmuxerr.New(pmuxerr.CodeAuth, pmuxerr.Upstream, "rejected"), 5},
		{"untyped internal", errors.New("boom"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			spy := &commandSpy{err: tc.err}
			code := execute(context.Background(), testDependencies(spy, false, out, stderr), []string{"--json", "models", "list"})
			if code != tc.exit {
				t.Fatalf("exit=%d want=%d", code, tc.exit)
			}
			var envelope struct {
				OK    bool      `json:"ok"`
				Error errorView `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatalf("invalid JSON %q: %v", out, err)
			}
			if envelope.OK || envelope.Error.Code == "" || envelope.Error.Message == "" {
				t.Fatalf("envelope=%+v", envelope)
			}
			if stderr.Len() != 0 {
				t.Fatalf("JSON error leaked to stderr: %s", stderr)
			}
		})
	}
}

func TestUnavailableCompositionFailsClosed(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := execute(context.Background(), testDependencies(nil, false, out, stderr), []string{"models", "list"})
	if code != 4 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, out, stderr)
	}
	if !strings.Contains(stderr.String(), "application services are unavailable") {
		t.Fatalf("imprecise failure: %s", stderr)
	}
}

func TestClaudeAliasProducesCanonicalLaunchInvocation(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{result: app.Result{Data: map[string]any{}}}
	code := execute(context.Background(), testDependencies(spy, false, out, stderr), []string{"claude", "runtime-model", "--", "--permission-mode", "plan"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	got := spy.calls[0]
	if got.Operation != "launch" || !reflect.DeepEqual(got.Arguments, []string{"--permission-mode", "plan"}) {
		t.Fatalf("invocation=%#v", got)
	}
	if got.Options["client"] != "claude" || got.Options["model"] != "runtime-model" {
		t.Fatalf("options=%#v", got.Options)
	}
}

func TestConfigAndLogFacadeMapsEveryAdvertisedArgument(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		op    app.Operation
		check func(t *testing.T, invocation app.Invocation)
	}{
		{
			name: "logs", op: app.OpServiceLogs,
			args: []string{"service", "logs", "--source", "proxy", "--level", "error", "--lines", "7", "--since", "5m", "--follow", "--output", "/tmp/private.ndjson"},
			check: func(t *testing.T, got app.Invocation) {
				if got.Options["source"] != "proxy" || got.Options["level"] != "error" || got.Options["lines"] != 7 || got.Options["since"] != "5m" || got.Options["follow"] != true || got.Options["output"] != "/tmp/private.ndjson" {
					t.Fatalf("log invocation=%#v", got)
				}
			},
		},
		{
			name: "clear logs", op: app.OpServiceLogs,
			args: []string{"--yes", "service", "logs", "--clear", "proxy"},
			check: func(t *testing.T, got app.Invocation) {
				if got.Options["clear"] != "proxy" || !got.Yes {
					t.Fatalf("clear invocation=%#v", got)
				}
			},
		},
		{
			name: "effective show", op: app.OpConfigShow,
			args: []string{"config", "--scope", "proxy", "show", "--effective", "--reveal-paths"},
			check: func(t *testing.T, got app.Invocation) {
				if got.Options["scope"] != "proxy" || got.Options["effective"] != true || got.Options["reveal_paths"] != true {
					t.Fatalf("show invocation=%#v", got)
				}
			},
		},
		{
			name: "persistent slot", op: app.OpConfigSet,
			args: []string{"--yes", "config", "--scope", "pmux", "set", "integrations.claude.persistent-models.opus", "exact-model"},
			check: func(t *testing.T, got app.Invocation) {
				if !reflect.DeepEqual(got.Arguments, []string{"integrations.claude.persistent-models.opus", "exact-model"}) || got.Options["scope"] != "pmux" {
					t.Fatalf("slot invocation=%#v", got)
				}
			},
		},
		{
			name: "edit", op: app.OpConfigEdit,
			args: []string{"config", "--scope", "proxy", "edit", "--editor", "/usr/bin/vi", "--restart"},
			check: func(t *testing.T, got app.Invocation) {
				if got.Options["editor"] != "/usr/bin/vi" || got.Options["restart"] != true || !got.Interactive {
					t.Fatalf("edit invocation=%#v", got)
				}
			},
		},
		{
			name: "restore", op: app.OpConfigRestore,
			args: []string{"--yes", "config", "restore", "backup-id", "--restart"},
			check: func(t *testing.T, got app.Invocation) {
				if !reflect.DeepEqual(got.Arguments, []string{"backup-id"}) || got.Options["restart"] != true {
					t.Fatalf("restore invocation=%#v", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			spy := &commandSpy{result: app.Result{Data: map[string]any{}}}
			code := execute(context.Background(), testDependencies(spy, true, out, stderr), tc.args)
			if code != 0 || len(spy.calls) != 1 {
				t.Fatalf("exit=%d calls=%d output=%s stderr=%s", code, len(spy.calls), out, stderr)
			}
			if spy.calls[0].Operation != tc.op {
				t.Fatalf("operation=%s want=%s", spy.calls[0].Operation, tc.op)
			}
			tc.check(t, spy.calls[0])
		})
	}
}

func TestLogClearRejectsReadAndStreamingFlagsBeforeDispatch(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{result: app.Result{Data: map[string]any{}}}
	code := execute(context.Background(), testDependencies(spy, false, out, stderr), []string{"--yes", "service", "logs", "--clear", "proxy", "--follow"})
	if code != 2 {
		t.Fatalf("exit=%d output=%s stderr=%s", code, out, stderr)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("invalid clear dispatched %d call(s)", len(spy.calls))
	}
}

func TestCompletionIsBuiltInForEveryCanonicalShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			out := &bytes.Buffer{}
			spy := &commandSpy{err: errors.New("completion must not call application services")}
			code := execute(context.Background(), testDependencies(spy, false, out, &bytes.Buffer{}), []string{"completion", shell})
			if code != 0 || out.Len() == 0 {
				t.Fatalf("exit=%d output=%q", code, out)
			}
			if len(spy.calls) != 0 {
				t.Fatal("completion called application use cases")
			}
		})
	}
}

func TestConfigDirArgHonorsFlagBoundaryAndLastValue(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "absent", args: []string{"version"}},
		{name: "separate", args: []string{"--config-dir", "/first", "version"}, want: "/first"},
		{name: "equals", args: []string{"version", "--config-dir=/equals"}, want: "/equals"},
		{name: "last wins", args: []string{"--config-dir=/first", "--config-dir", "/last"}, want: "/last"},
		{name: "client passthrough is ignored", args: []string{"claude", "model", "--", "--config-dir", "/client"}, want: ""},
		{name: "missing value", args: []string{"version", "--config-dir"}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := configDirArg(test.args); got != test.want {
				t.Fatalf("configDirArg(%q) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

func assertFlags(t *testing.T, command *cobra.Command, persistent bool, expected []string) {
	t.Helper()
	flags := command.LocalNonPersistentFlags()
	if persistent {
		flags = command.PersistentFlags()
	}
	for _, name := range expected {
		if flags.Lookup(name) == nil {
			t.Errorf("%s missing --%s", command.CommandPath(), name)
		}
	}
}

func assertChildren(t *testing.T, parent *cobra.Command, expected []string) {
	t.Helper()
	actual := make([]string, 0, len(parent.Commands()))
	for _, command := range parent.Commands() {
		if !command.Hidden {
			actual = append(actual, command.Name())
		}
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s children=%v want=%v", parent.CommandPath(), actual, expected)
	}
}

func child(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	command := findChild(parent, name)
	if command == nil {
		t.Fatalf("%s missing child %q", parent.CommandPath(), name)
	}
	return command
}

func findChild(parent *cobra.Command, name string) *cobra.Command {
	for _, command := range parent.Commands() {
		if command.Name() == name {
			return command
		}
	}
	return nil
}

func walk(root *cobra.Command, visit func(*cobra.Command)) {
	visit(root)
	for _, command := range root.Commands() {
		walk(command, visit)
	}
}
