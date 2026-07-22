package configfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func templateConfig(authDir string) string {
	return `# operator-owned header
host: "0.0.0.0" # keep host note
port: 8317
auth-dir: ` + authDir + `
api-keys:
  - example-api-key # upstream template
remote-management:
  allow-remote: true # keep management note
unknown-before: untouched
provider: &provider
  base-url: https://example.invalid
  api-key: provider-secret-must-survive
provider-copy: *provider
unknown-after: 42 # keep tail
`
}

func testAuthDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "auth")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManagedHardeningPreservesASTAndAppliesPrivately(t *testing.T) {
	authDir := testAuthDir(t)
	original := templateConfig(authDir)
	path := writeFixture(t, original)
	backups := filepath.Join(t.TempDir(), "backups", "default")
	adapter := New(backups)
	adapter.now = func() time.Time { return time.Date(2026, 7, 20, 14, 12, 3, 0, time.UTC) }
	adapter.random = bytes.NewReader(bytes.Repeat([]byte{0xab}, 32))

	snapshot, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.PlanManagedHardening(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	generated := "sk-" + strings.Repeat("ab", 32)
	if strings.Contains(plan.Diff, generated) || strings.Contains(plan.Diff, "example-api-key") {
		t.Fatalf("redacted diff disclosed a key: %q", plan.Diff)
	}
	if !strings.Contains(plan.Diff, "api-keys: <redacted>") {
		t.Fatalf("diff did not identify redacted key change: %q", plan.Diff)
	}
	if !plan.RestartRequired {
		t.Fatal("host hardening must be classified restart-required")
	}

	candidate := string(plan.Rendered)
	for _, preserved := range []string{
		"# operator-owned header", `host: "127.0.0.1" # keep host note`, "# keep management note",
		"unknown-before: untouched", "provider: &provider", "provider-copy: *provider",
		"provider-secret-must-survive", "unknown-after: 42 # keep tail",
	} {
		if !strings.Contains(candidate, preserved) {
			t.Errorf("candidate did not preserve %q:\n%s", preserved, candidate)
		}
	}
	if strings.Index(candidate, "unknown-before:") > strings.Index(candidate, "unknown-after:") {
		t.Fatal("unrelated mapping order changed")
	}
	if !strings.Contains(candidate, generated) || strings.Contains(candidate, "example-api-key") {
		t.Fatal("hardening did not replace the sole template key")
	}

	result, err := adapter.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(result.BackupPath), "config.yaml.20260720T141203Z."+shortSHA([]byte(original))+".bak"; got != want {
		t.Fatalf("backup name = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		for _, privatePath := range []string{path, result.BackupPath} {
			info, err := os.Stat(privatePath)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Errorf("%s mode = %o, want 600", privatePath, got)
			}
		}
		backupInfo, err := os.Stat(backups)
		if err != nil {
			t.Fatal(err)
		}
		if got := backupInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("backup directory mode = %o, want 700", got)
		}
	} else {
		platform, err := adapterplatform.New("")
		if err != nil {
			t.Fatal(err)
		}
		for _, privatePath := range []string{path, result.BackupPath} {
			if err := platform.VerifySecurePermissions(privatePath, false); err != nil {
				t.Errorf("%s permissions: %v", privatePath, err)
			}
		}
		if err := platform.VerifySecurePermissions(backups, true); err != nil {
			t.Errorf("backup directory permissions: %v", err)
		}
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != original {
		t.Fatal("backup does not contain exact prior bytes")
	}
	applied, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Config.Host != "127.0.0.1" || !applied.Config.WSAuth || !applied.Config.ManagementLocal {
		t.Fatalf("managed hardening missing: %+v", applied.Config)
	}
	if len(applied.Config.APIKeys) != 1 || applied.Config.APIKeys[0] != generated {
		t.Fatal("managed key was not committed exactly once")
	}
	for _, diagnostic := range adapter.Validate(context.Background(), applied) {
		if diagnostic.ID == "KEY-SAFEMODE" || diagnostic.ID == "KEY-PERMS" || diagnostic.ID == "MGMT-LOCAL" || diagnostic.ID == "CFG-WSAUTH" || diagnostic.ID == "SEC-EXPOSURE" {
			t.Errorf("unexpected post-hardening diagnostic: %+v", diagnostic)
		}
	}
}

func TestPlanAndApplyRejectStaleFingerprint(t *testing.T) {
	path := writeFixture(t, secureConfig(t, "8317"))
	adapter := New(filepath.Join(t.TempDir(), "backups"))
	snapshot, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(secureConfig(t, "8318")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{Path: "ws-auth", Value: true}})
	assertPMuxCode(t, err, pmuxerr.ConfigMutationConflict)

	fresh, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.Plan(context.Background(), fresh, []domainconfig.PatchOp{{Path: "port", Value: 8319}})
	if err != nil {
		t.Fatal(err)
	}
	changed := secureConfig(t, "8320")
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Apply(context.Background(), plan)
	assertPMuxCode(t, err, pmuxerr.ConfigMutationConflict)
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != changed {
		t.Fatal("stale apply changed the file")
	}
}

func TestApplyRollsBackAfterVerificationFailure(t *testing.T) {
	original := secureConfig(t, "8317")
	path := writeFixture(t, original)
	backupDir := filepath.Join(t.TempDir(), "backups")
	adapter := New(backupDir)
	adapter.now = func() time.Time { return time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC) }
	snapshot, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{Path: "port", Value: 9000}})
	if err != nil {
		t.Fatal(err)
	}
	adapter.afterRename = func(string) error { return errors.New("injected verification failure") }
	_, err = adapter.Apply(context.Background(), plan)
	assertPMuxCode(t, err, pmuxerr.ConfigValidationFailed)
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Fatalf("rollback did not restore exact prior bytes:\n%s", got)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup count = %d, want 1", len(entries))
	}
}

func TestTemplateKeyDetectionAndErrorsNeverRevealKey(t *testing.T) {
	const template = "sk-placeholder-super-secret-canary"
	path := writeFixture(t, strings.Replace(secureConfig(t, "8317"), "sk-real-key-abcdefghijklmnopqrstuvwxyz", template, 1))
	adapter := New(filepath.Join(t.TempDir(), "backups"))
	snapshot, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := adapter.Validate(context.Background(), snapshot)
	if !hasDiagnostic(diagnostics, "KEY-SAFEMODE") {
		t.Fatalf("safe mode was not diagnosed: %+v", diagnostics)
	}
	_, err = adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{Path: "ws-auth", Value: true}})
	assertPMuxCode(t, err, pmuxerr.ConfigSafeMode)
	var structured *pmuxerr.Error
	if !errors.As(err, &structured) {
		t.Fatalf("error is not pmuxerr.Error: %T", err)
	}
	joined := structured.Error() + strings.Join(structured.Evidence, " ") + strings.Join(structured.Repair, " ")
	if strings.Contains(joined, template) {
		t.Fatalf("error disclosed template key: %q", joined)
	}
}

func TestPlanKeepsManagementLocalAndRedactsStructuredSecrets(t *testing.T) {
	path := writeFixture(t, secureConfig(t, "8317"))
	adapter := New(filepath.Join(t.TempDir(), "backups"))
	snapshot, err := adapter.Read(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{
		Path: "remote-management.allow-remote", Value: true,
	}})
	assertPMuxCode(t, err, pmuxerr.ConfigValidationFailed)

	const storeURL = "postgres://operator:secret-canary@localhost/db"
	plan, err := adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{
		Path: "pgstore.url", Value: storeURL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Diff, storeURL) || strings.Contains(plan.Diff, "secret-canary") {
		t.Fatalf("structured secret leaked in diff: %q", plan.Diff)
	}
	if !strings.Contains(plan.Diff, "pgstore.url: <redacted>") {
		t.Fatalf("structured change was not marked redacted: %q", plan.Diff)
	}
}

func TestRestartRequiredClassification(t *testing.T) {
	tests := []struct {
		name string
		op   domainconfig.PatchOp
		want bool
	}{
		{"hot ws auth", domainconfig.PatchOp{Path: "ws-auth", Value: true}, false},
		{"hot api keys", domainconfig.PatchOp{Path: "api-keys", Value: []string{"sk-another-real-key-abcdefghijklmnopqrstuvwxyz"}}, false},
		{"host", domainconfig.PatchOp{Path: "host", Value: "localhost"}, true},
		{"port", domainconfig.PatchOp{Path: "port", Value: 9000}, true},
		{"tls", domainconfig.PatchOp{Path: "tls.enable", Value: true}, true},
		{"token store", domainconfig.PatchOp{Path: "pgstore.url", Value: "postgres://localhost/db"}, true},
		{"plugin", domainconfig.PatchOp{Path: "plugins.example.path", Value: "/tmp/plugin.so"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFixture(t, secureConfig(t, "8317"))
			adapter := New(filepath.Join(t.TempDir(), "backups"))
			snapshot, err := adapter.Read(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{tt.op})
			if err != nil {
				t.Fatal(err)
			}
			if plan.RestartRequired != tt.want {
				t.Fatalf("RestartRequired = %v, want %v", plan.RestartRequired, tt.want)
			}
		})
	}
}

func TestPlanExtendedConfigSurface(t *testing.T) {
	boolValue := true
	tests := []struct {
		name        string
		op          domainconfig.PatchOp
		wantErr     bool
		wantRestart bool
	}{
		// routing
		{"strategy round-robin", domainconfig.PatchOp{Path: "routing.strategy", Value: "round-robin"}, false, false},
		{"strategy fill-first", domainconfig.PatchOp{Path: "routing.strategy", Value: "fill-first"}, false, false},
		{"strategy unknown enum", domainconfig.PatchOp{Path: "routing.strategy", Value: "random"}, true, false},
		{"strategy wrong type", domainconfig.PatchOp{Path: "routing.strategy", Value: 1}, true, false},
		{"session affinity", domainconfig.PatchOp{Path: "routing.session-affinity", Value: boolValue}, false, false},
		{"session affinity wrong type", domainconfig.PatchOp{Path: "routing.session-affinity", Value: "true"}, true, false},
		{"session affinity ttl", domainconfig.PatchOp{Path: "routing.session-affinity-ttl", Value: "1h"}, false, false},
		{"session affinity ttl invalid", domainconfig.PatchOp{Path: "routing.session-affinity-ttl", Value: "not-a-duration"}, true, false},
		{"session affinity ttl wrong type", domainconfig.PatchOp{Path: "routing.session-affinity-ttl", Value: 60}, true, false},
		// quota-exceeded
		{"quota switch project", domainconfig.PatchOp{Path: "quota-exceeded.switch-project", Value: boolValue}, false, false},
		{"quota switch project wrong type", domainconfig.PatchOp{Path: "quota-exceeded.switch-project", Value: 1}, true, false},
		{"quota switch preview model", domainconfig.PatchOp{Path: "quota-exceeded.switch-preview-model", Value: false}, false, false},
		{"quota switch preview model wrong type", domainconfig.PatchOp{Path: "quota-exceeded.switch-preview-model", Value: "yes"}, true, false},
		{"quota antigravity credits", domainconfig.PatchOp{Path: "quota-exceeded.antigravity-credits", Value: boolValue}, false, false},
		{"quota antigravity credits wrong type", domainconfig.PatchOp{Path: "quota-exceeded.antigravity-credits", Value: 0}, true, false},
		// non-negative integers
		{"request retry", domainconfig.PatchOp{Path: "request-retry", Value: 3}, false, false},
		{"request retry negative", domainconfig.PatchOp{Path: "request-retry", Value: -1}, true, false},
		{"request retry wrong type", domainconfig.PatchOp{Path: "request-retry", Value: "3"}, true, false},
		{"max retry interval", domainconfig.PatchOp{Path: "max-retry-interval", Value: 30}, false, false},
		{"max retry interval negative", domainconfig.PatchOp{Path: "max-retry-interval", Value: -2}, true, false},
		{"max retry credentials", domainconfig.PatchOp{Path: "max-retry-credentials", Value: 0}, false, false},
		{"max retry credentials wrong type", domainconfig.PatchOp{Path: "max-retry-credentials", Value: 1.5}, true, false},
		{"nonstream keepalive", domainconfig.PatchOp{Path: "nonstream-keepalive-interval", Value: 15}, false, false},
		{"nonstream keepalive negative", domainconfig.PatchOp{Path: "nonstream-keepalive-interval", Value: -1}, true, false},
		{"streaming keepalive", domainconfig.PatchOp{Path: "streaming.keepalive-seconds", Value: 10}, false, false},
		{"streaming keepalive negative", domainconfig.PatchOp{Path: "streaming.keepalive-seconds", Value: -5}, true, false},
		{"streaming bootstrap retries", domainconfig.PatchOp{Path: "streaming.bootstrap-retries", Value: 2}, false, false},
		{"streaming bootstrap retries wrong type", domainconfig.PatchOp{Path: "streaming.bootstrap-retries", Value: boolValue}, true, false},
		{"logs max total size", domainconfig.PatchOp{Path: "logs-max-total-size-mb", Value: 512}, false, false},
		{"logs max total size negative", domainconfig.PatchOp{Path: "logs-max-total-size-mb", Value: -1}, true, false},
		{"error logs max files", domainconfig.PatchOp{Path: "error-logs-max-files", Value: 20}, false, false},
		{"error logs max files wrong type", domainconfig.PatchOp{Path: "error-logs-max-files", Value: "20"}, true, false},
		// transient-error-cooldown-seconds allows -1
		{"transient cooldown minus one", domainconfig.PatchOp{Path: "transient-error-cooldown-seconds", Value: -1}, false, false},
		{"transient cooldown zero", domainconfig.PatchOp{Path: "transient-error-cooldown-seconds", Value: 0}, false, false},
		{"transient cooldown too low", domainconfig.PatchOp{Path: "transient-error-cooldown-seconds", Value: -2}, true, false},
		// boolean toggles
		{"codex identity confuse", domainconfig.PatchOp{Path: "codex.identity-confuse", Value: boolValue}, false, false},
		{"codex identity confuse wrong type", domainconfig.PatchOp{Path: "codex.identity-confuse", Value: "true"}, true, false},
		{"passthrough headers", domainconfig.PatchOp{Path: "passthrough-headers", Value: boolValue}, false, false},
		{"commercial mode", domainconfig.PatchOp{Path: "commercial-mode", Value: false}, false, false},
		{"debug", domainconfig.PatchOp{Path: "debug", Value: boolValue}, false, false},
		{"debug wrong type", domainconfig.PatchOp{Path: "debug", Value: 1}, true, false},
		{"logging to file", domainconfig.PatchOp{Path: "logging-to-file", Value: boolValue}, false, false},
		{"usage statistics", domainconfig.PatchOp{Path: "usage-statistics-enabled", Value: false}, false, false},
		{"request log", domainconfig.PatchOp{Path: "request-log", Value: boolValue}, false, false},
		{"force model prefix", domainconfig.PatchOp{Path: "force-model-prefix", Value: false}, false, false},
		{"disable cooling", domainconfig.PatchOp{Path: "disable-cooling", Value: boolValue}, false, false},
		{"save cooldown status", domainconfig.PatchOp{Path: "save-cooldown-status", Value: boolValue}, false, false},
		{"disable claude cloak mode", domainconfig.PatchOp{Path: "disable-claude-cloak-mode", Value: false}, false, false},
		{"disable claude cloak mode wrong type", domainconfig.PatchOp{Path: "disable-claude-cloak-mode", Value: "no"}, true, false},
		// proxy-url
		{"proxy url http", domainconfig.PatchOp{Path: "proxy-url", Value: "http://proxy.internal:3128"}, false, false},
		{"proxy url https", domainconfig.PatchOp{Path: "proxy-url", Value: "https://user:pass@proxy.internal:8443"}, false, false},
		{"proxy url socks5", domainconfig.PatchOp{Path: "proxy-url", Value: "socks5://127.0.0.1:1080"}, false, false},
		{"proxy url empty", domainconfig.PatchOp{Path: "proxy-url", Value: ""}, false, false},
		{"proxy url relative", domainconfig.PatchOp{Path: "proxy-url", Value: "proxy.internal:3128"}, true, false},
		{"proxy url bad scheme", domainconfig.PatchOp{Path: "proxy-url", Value: "ftp://proxy.internal:21"}, true, false},
		{"proxy url wrong type", domainconfig.PatchOp{Path: "proxy-url", Value: 3128}, true, false},
		// disable-image-generation union
		{"image generation bool", domainconfig.PatchOp{Path: "disable-image-generation", Value: boolValue}, false, false},
		{"image generation chat", domainconfig.PatchOp{Path: "disable-image-generation", Value: "chat"}, false, false},
		{"image generation passthrough", domainconfig.PatchOp{Path: "disable-image-generation", Value: "passthrough"}, false, false},
		{"image generation bad enum", domainconfig.PatchOp{Path: "disable-image-generation", Value: "off"}, true, false},
		{"image generation wrong type", domainconfig.PatchOp{Path: "disable-image-generation", Value: 1}, true, false},
		// durations
		{"video cache ttl", domainconfig.PatchOp{Path: "video-result-auth-cache-ttl", Value: "24h"}, false, false},
		{"video cache ttl invalid", domainconfig.PatchOp{Path: "video-result-auth-cache-ttl", Value: "forever"}, true, false},
		// remote management panel repository
		{"panel github repository", domainconfig.PatchOp{Path: "remote-management.panel-github-repository", Value: "owner/repo"}, false, false},
		{"panel github repository wrong type", domainconfig.PatchOp{Path: "remote-management.panel-github-repository", Value: 7}, true, false},
		// payload sequences
		{"payload rules", domainconfig.PatchOp{Path: "payload", Value: []any{map[string]any{"models": []any{"gpt-5"}}}}, false, false},
		{"payload wrong type", domainconfig.PatchOp{Path: "payload", Value: map[string]any{}}, true, false},
		{"payload default", domainconfig.PatchOp{Path: "payload.default", Value: []any{}}, false, false},
		{"payload default wrong type", domainconfig.PatchOp{Path: "payload.default", Value: "rule"}, true, false},
		{"payload default raw", domainconfig.PatchOp{Path: "payload.default-raw", Value: []any{map[string]any{"op": "set"}}}, false, false},
		{"payload override", domainconfig.PatchOp{Path: "payload.override", Value: []any{}}, false, false},
		{"payload override raw", domainconfig.PatchOp{Path: "payload.override-raw", Value: []any{}}, false, false},
		{"payload filter", domainconfig.PatchOp{Path: "payload.filter", Value: []any{"thinking"}}, false, false},
		{"payload filter wrong type", domainconfig.PatchOp{Path: "payload.filter", Value: 3}, true, false},
		// pprof is restart-required
		{"pprof", domainconfig.PatchOp{Path: "pprof", Value: boolValue}, false, true},
		{"pprof wrong type", domainconfig.PatchOp{Path: "pprof", Value: "true"}, true, true},
		{"pprof enable", domainconfig.PatchOp{Path: "pprof.enable", Value: boolValue}, false, true},
		{"pprof enable wrong type", domainconfig.PatchOp{Path: "pprof.enable", Value: 1}, true, true},
		{"pprof addr", domainconfig.PatchOp{Path: "pprof.addr", Value: "127.0.0.1:6060"}, false, true},
		{"pprof addr missing port", domainconfig.PatchOp{Path: "pprof.addr", Value: "127.0.0.1"}, true, true},
		{"pprof addr wrong type", domainconfig.PatchOp{Path: "pprof.addr", Value: 6060}, true, true},
		// header-defaults subtrees are hot-reloaded
		{"claude header defaults root", domainconfig.PatchOp{Path: "claude-header-defaults", Value: map[string]any{"x-anthropic-beta": "oauth"}}, false, false},
		{"claude header defaults subtree", domainconfig.PatchOp{Path: "claude-header-defaults.x-api-key", Value: "value"}, false, false},
		{"codex header defaults root", domainconfig.PatchOp{Path: "codex-header-defaults", Value: map[string]any{"originator": "pmux"}}, false, false},
		{"codex header defaults subtree", domainconfig.PatchOp{Path: "codex-header-defaults.session_id", Value: "abc"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFixture(t, secureConfig(t, "8317"))
			adapter := New(filepath.Join(t.TempDir(), "backups"))
			snapshot, err := adapter.Read(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{tt.op})
			if tt.wantErr {
				assertPMuxCode(t, err, pmuxerr.ConfigValidationFailed)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.RestartRequired != tt.wantRestart {
				t.Fatalf("RestartRequired = %v, want %v", plan.RestartRequired, tt.wantRestart)
			}
		})
	}
}

func TestPlanRejectsUnknownExtendedPaths(t *testing.T) {
	for _, path := range []string{
		"routing.unknown",
		"quota-exceeded.unknown",
		"streaming.unknown",
		"payload.default.0",
		"payload.unknown",
		"pprof.extra",
		"codex.unknown",
		"header-defaults",
	} {
		t.Run(path, func(t *testing.T) {
			fixture := writeFixture(t, secureConfig(t, "8317"))
			adapter := New(filepath.Join(t.TempDir(), "backups"))
			snapshot, err := adapter.Read(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{Path: path, Value: true}})
			assertPMuxCode(t, err, pmuxerr.ConfigValidationFailed)
		})
	}
}

func TestPlanUnsetExtendedPaths(t *testing.T) {
	// Unset skips value validation for every known path, including new ones.
	for _, path := range []string{
		"routing.strategy", "quota-exceeded.switch-project", "request-retry",
		"proxy-url", "payload.default", "pprof.addr", "claude-header-defaults.x",
	} {
		t.Run(path, func(t *testing.T) {
			fixture := writeFixture(t, secureConfig(t, "8317"))
			adapter := New(filepath.Join(t.TempDir(), "backups"))
			snapshot, err := adapter.Read(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := adapter.Plan(context.Background(), snapshot, []domainconfig.PatchOp{{Path: path, Unset: true}})
			if err != nil {
				t.Fatal(err)
			}
			wantRestart := strings.HasPrefix(path, "pprof")
			if plan.RestartRequired != wantRestart {
				t.Fatalf("RestartRequired = %v, want %v", plan.RestartRequired, wantRestart)
			}
		})
	}
}

func TestReadRejectsDuplicateMappingKeys(t *testing.T) {
	path := writeFixture(t, secureConfig(t, "8317")+"port: 9000\n")
	_, err := New(filepath.Join(t.TempDir(), "backups")).Read(context.Background(), path)
	assertPMuxCode(t, err, pmuxerr.ConfigUnreadable)
}

func TestGeneratedProxyKeyUsesExactly32RandomBytes(t *testing.T) {
	reader := &countingReader{Reader: bytes.NewReader(bytes.Repeat([]byte{0x7f}, 32))}
	key, err := generateProxyKey(reader)
	if err != nil {
		t.Fatal(err)
	}
	if reader.count != 32 {
		t.Fatalf("random byte count = %d, want 32", reader.count)
	}
	if len(key) != 3+64 || !strings.HasPrefix(key, "sk-") {
		t.Fatalf("generated key has wrong shape: %q", key)
	}
}

func secureConfig(t *testing.T, port string) string {
	t.Helper()
	return "host: 127.0.0.1\n" +
		"port: " + port + "\n" +
		"auth-dir: " + testAuthDir(t) + "\n" +
		"api-keys:\n  - sk-real-key-abcdefghijklmnopqrstuvwxyz\n" +
		"remote-management:\n  allow-remote: false\n" +
		"ws-auth: true\n"
}

func shortSHA(body []byte) string {
	sum := sha256ForTest(body)
	return sum[:8]
}

func sha256ForTest(body []byte) string {
	// yaml adapter uses the standard SHA-256 lower-hex representation.
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func hasDiagnostic(diagnostics []domainconfig.Diagnostic, id string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.ID == id {
			return true
		}
	}
	return false
}

func assertPMuxCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	var structured *pmuxerr.Error
	if !errors.As(err, &structured) {
		t.Fatalf("error %T is not pmuxerr.Error: %v", err, err)
	}
	if structured.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", structured.Code, code, err)
	}
}

type countingReader struct {
	*bytes.Reader
	count int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count += n
	return n, err
}

// Compile-time guard for the transport-neutral domain contract.
var _ domainconfig.ConfigFile = (*Adapter)(nil)
