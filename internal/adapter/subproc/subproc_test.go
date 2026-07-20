package subproc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

func TestLoginArgsClosedExactMap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id        management.ProviderID
		flow      provider.AuthFlow
		noBrowser bool
		want      []string
	}{
		{"codex", provider.FlowBrowser, true, []string{"-codex-login", "-no-browser"}},
		{"codex", provider.FlowDeviceCode, true, []string{"-codex-device-login"}},
		{"claude", provider.FlowBrowser, true, []string{"-claude-login", "-no-browser"}},
		{"anthropic", provider.FlowBrowser, false, []string{"-claude-login"}},
		{"antigravity", provider.FlowBrowser, true, []string{"-antigravity-login", "-no-browser"}},
		{"kimi", provider.FlowDeviceCode, true, []string{"-kimi-login"}},
		{"xai", provider.FlowDeviceCode, true, []string{"-xai-login"}},
	}
	for _, test := range cases {
		test := test
		t.Run(string(test.id)+"/"+string(test.flow), func(t *testing.T) {
			got, err := LoginArgs(test.id, test.flow, test.noBrowser)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
	for _, id := range []management.ProviderID{"qwen", "iflow", "github-copilot", "made-up"} {
		if flags, err := LoginArgs(id, provider.FlowBrowser, true); err == nil || flags != nil {
			t.Fatalf("unsupported provider %q produced flags %#v, error %v", id, flags, err)
		}
	}
}

func TestScrubbedEnvironmentIsAllowlist(t *testing.T) {
	t.Parallel()
	parent := []string{
		"PATH=/bin", "HOME=/home/test", "TERM=xterm", "LANG=C", "LC_ALL=C",
		"PGSTORE_DSN=secret", "OBJECTSTORE_URL=secret", "GITSTORE_TOKEN=secret",
		"ANTHROPIC_AUTH_TOKEN=secret", "OPENAI_API_KEY=secret", "GEMINI_API_KEY=secret",
		"MANAGEMENT_PASSWORD=secret", "PMUX_SECRET=secret", "HTTP_PROXY=http://inherited.invalid",
	}
	got := ScrubbedEnvironment(parent, EnvironmentOptions{
		GOOS:          "linux",
		OutboundProxy: map[string]string{"HTTPS_PROXY": "http://configured.test", "NO_PROXY": "127.0.0.1"},
		TLS:           map[string]string{"SSL_CERT_FILE": "/private/ca.pem"},
	})
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"secret", "PGSTORE_", "OBJECTSTORE_", "GITSTORE_", "ANTHROPIC_", "OPENAI_", "GEMINI_", "MANAGEMENT_PASSWORD", "PMUX_", "inherited.invalid"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("environment leaked %q:\n%s", forbidden, joined)
		}
	}
	for _, expected := range []string{"HOME=/home/test", "PATH=/bin", "HTTPS_PROXY=http://configured.test", "NO_PROXY=127.0.0.1", "SSL_CERT_FILE=/private/ca.pem"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("environment missing %q:\n%s", expected, joined)
		}
	}
}

func TestBuildInvocationAlwaysUsesAbsoluteConfigAndCleanRuntime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "cli-proxy-api")
	config := filepath.Join(root, "config.yaml")
	runtimeDir := filepath.Join(root, "runtime")
	mustWrite(t, binary, "binary", 0o700)
	mustWrite(t, config, "host: 127.0.0.1", 0o600)
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	invocation, err := BuildInvocation(binary, config, runtimeDir, []string{"-kimi-login"}, []string{"PATH=/bin"}, EnvironmentOptions{GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(invocation.Path) || !filepath.IsAbs(invocation.Dir) {
		t.Fatalf("paths are not absolute: %#v", invocation)
	}
	if len(invocation.Args) != 3 || invocation.Args[0] != "-kimi-login" || invocation.Args[1] != "-config" || invocation.Args[2] != config || !filepath.IsAbs(invocation.Args[2]) {
		t.Fatalf("unexpected argv: %#v", invocation.Args)
	}
	mustWrite(t, filepath.Join(runtimeDir, ".env"), "PGSTORE_DSN=bad", 0o600)
	_, err = BuildInvocation(binary, config, runtimeDir, nil, nil, EnvironmentOptions{})
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.ConfigPathMismatch {
		t.Fatalf("expected config-path error, got %v", err)
	}
}

type inspectingExecutor struct {
	t          *testing.T
	invocation Invocation
	config     string
	authDir    string
	port       int
}

func (e *inspectingExecutor) Execute(_ context.Context, invocation Invocation, observe func(string) bool) error {
	e.invocation = invocation
	bytes, err := os.ReadFile(invocation.Args[len(invocation.Args)-1])
	if err != nil {
		e.t.Fatal(err)
	}
	e.config = string(bytes)
	for _, line := range strings.Split(e.config, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "auth-dir:"):
			e.authDir = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "auth-dir:")), "\"")
		case strings.HasPrefix(line, "port:"):
			e.port, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "port:")))
		}
	}
	entries, err := os.ReadDir(e.authDir)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(entries) != 0 {
		e.t.Fatalf("probe auth dir is not empty: %#v", entries)
	}
	observe("unrelated startup line")
	observe("CLIProxyAPI Version: 7.2.92, Commit: abc123, BuiltAt: 2026-07-20T00:00:00Z")
	return nil
}

func TestVersionProbeUsesGeneratedIsolatedState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "cli-proxy-api")
	mustWrite(t, binary, "not actually executed", 0o700)
	userConfig := filepath.Join(root, "user-config.yaml")
	mustWrite(t, userConfig, "secret-key: should-never-be-read", 0o600)
	before, err := os.ReadFile(userConfig)
	if err != nil {
		t.Fatal(err)
	}
	executor := &inspectingExecutor{t: t}
	probe := VersionProbe{
		Executor:   executor,
		TempRoot:   root,
		ParentEnv:  []string{"PATH=/bin", "PGSTORE_DSN=forbidden", "MANAGEMENT_PASSWORD=forbidden", "ANTHROPIC_AUTH_TOKEN=forbidden"},
		EnvOptions: EnvironmentOptions{GOOS: "linux"},
	}
	info, err := probe.Probe(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "7.2.92" || info.Commit != "abc123" {
		t.Fatalf("unexpected version: %#v", info)
	}
	if strings.Contains(executor.config, string(before)) || strings.Contains(strings.Join(executor.invocation.Args, " "), userConfig) {
		t.Fatal("isolated probe used the user's config")
	}
	for _, required := range []string{"host: 127.0.0.1", "ws-auth: true", "allow-remote: false", "sk-"} {
		if !strings.Contains(executor.config, required) {
			t.Fatalf("probe config missing %q:\n%s", required, executor.config)
		}
	}
	if executor.port <= 0 {
		t.Fatalf("probe did not allocate a free loopback port: %d", executor.port)
	}
	if filepath.Dir(executor.authDir) != executor.invocation.Dir {
		t.Fatalf("auth dir not isolated under runtime: %s vs %s", executor.authDir, executor.invocation.Dir)
	}
	if _, err := os.Stat(executor.invocation.Dir); !os.IsNotExist(err) {
		t.Fatalf("probe temp directory was not removed: %v", err)
	}
	if after, _ := os.ReadFile(userConfig); string(after) != string(before) {
		t.Fatal("user config changed during isolated probe")
	}
	joinedEnv := strings.Join(executor.invocation.Env, "\n")
	if strings.Contains(joinedEnv, "forbidden") {
		t.Fatalf("probe leaked parent secrets: %s", joinedEnv)
	}
}

func TestVersionProbeUnsafeBinaryFailsWithoutExecution(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "cli-proxy-api")
	mustWrite(t, binary, "not executable", 0o600)
	executor := &inspectingExecutor{t: t}
	_, err := (VersionProbe{Executor: executor, TempRoot: root}).Probe(context.Background(), binary)
	if !errors.Is(err, ErrUnsafeProbe) {
		t.Fatalf("expected unsafe probe, got %v", err)
	}
	if executor.invocation.Path != "" {
		t.Fatal("unsafe binary was executed")
	}
}

type captureExecutor struct {
	invocation Invocation
}

func (e *captureExecutor) Execute(_ context.Context, invocation Invocation, _ func(string) bool) error {
	e.invocation = invocation
	return nil
}

func TestAuthRunnerValidatesClosedModifiers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binary := filepath.Join(root, "cli-proxy-api")
	config := filepath.Join(root, "config.yaml")
	runtimeDir := filepath.Join(root, "runtime")
	mustWrite(t, binary, "binary", 0o700)
	mustWrite(t, config, "host: 127.0.0.1", 0o600)
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	executor := &captureExecutor{}
	runner := AuthRunner{BinaryPath: binary, ConfigPath: config, RuntimeDir: runtimeDir, Executor: executor}
	if err := runner.RunAuth(context.Background(), "codex", provider.FlowBrowser, []string{"-codex-login", "-no-browser"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(executor.invocation.Args, " ") != "-codex-login -no-browser -config "+config {
		t.Fatalf("unexpected callback argv: %#v", executor.invocation.Args)
	}
	serviceAccount := filepath.Join(root, "vertex.json")
	mustWrite(t, serviceAccount, "{}", 0o600)
	if err := runner.RunAuth(context.Background(), "vertex", provider.FlowVertexImport, []string{"-vertex-import", serviceAccount, "-vertex-import-prefix", "team"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(executor.invocation.Args, " ") != "-vertex-import "+serviceAccount+" -vertex-import-prefix team -config "+config {
		t.Fatalf("unexpected Vertex argv: %#v", executor.invocation.Args)
	}
	invalid := []struct {
		id    management.ProviderID
		flow  provider.AuthFlow
		flags []string
	}{
		{"codex", provider.FlowBrowser, []string{"-codex-login", "-evil"}},
		{"vertex", provider.FlowVertexImport, []string{"-vertex-import", "relative.json"}},
		{"qwen", provider.FlowBrowser, []string{"-qwen-login"}},
	}
	for _, test := range invalid {
		if err := runner.RunAuth(context.Background(), test.id, test.flow, test.flags); err == nil {
			t.Fatalf("unmapped flags were accepted: %#v", test.flags)
		}
	}
}

func TestProcessOutputRequiresSanitizerBeforeMirroring(t *testing.T) {
	t.Parallel()
	const secret = "sk-abcdefghijklmnopqrstuvwxyz"
	var withoutSanitizer bytes.Buffer
	lines := make(chan streamLine, 2)
	var wait sync.WaitGroup
	wait.Add(1)
	scanStream(strings.NewReader("token "+secret+"\n"), &withoutSanitizer, nil, lines, &wait)
	wait.Wait()
	if withoutSanitizer.Len() != 0 {
		t.Fatalf("raw subprocess output was mirrored without a sanitizer: %q", withoutSanitizer.String())
	}

	var sanitized bytes.Buffer
	lines = make(chan streamLine, 2)
	wait.Add(1)
	scanStream(strings.NewReader("token "+secret+"\n"), &sanitized, func(line string) string {
		return redact.Known(line, secret)
	}, lines, &wait)
	wait.Wait()
	if strings.Contains(sanitized.String(), secret) || sanitized.String() != "token <redacted>\n" {
		t.Fatalf("subprocess output was not safely mirrored: %q", sanitized.String())
	}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
