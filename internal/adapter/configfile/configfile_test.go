package configfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
