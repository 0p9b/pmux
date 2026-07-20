package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/adapter/configfile"
	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/state"
)

func TestSafeModeFixPreviewsAppliesAndRollsBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	configPath := filepath.Join(root, "instance", "config.yaml")
	keyPath := filepath.Join(root, "instance", "api-key.txt")
	original := []byte("host: 127.0.0.1\nport: 8317\nauth-dir: " + filepath.Join(root, "auth") + "\napi-keys:\n  - example-api-key\nws-auth: true\nremote-management:\n  allow-remote: false\n")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	platform, err := adapterplatform.New(filepath.Join(root, "config-root"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, waits := 0, 0
	fix := &safeModeFix{
		adapter:      configfile.New(filepath.Join(root, "backups")),
		installation: state.Installation{ConfigPath: configPath, ProxyKeyRef: state.SecretReference{Path: keyPath}},
		platform:     platform,
		verify: func(_ context.Context, key string) error {
			if key == "" {
				t.Fatal("empty key was verified")
			}
			attempts++
			if attempts < 3 {
				return errors.New("hot reload pending")
			}
			return nil
		},
		wait: func(ctx context.Context, duration time.Duration) error {
			if duration < time.Second {
				t.Fatalf("poll duration = %s", duration)
			}
			waits++
			return ctx.Err()
		},
	}
	preview, err := fix.Apply(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Changed || preview.Summary == "" {
		t.Fatalf("preview = %#v", preview)
	}
	if body, err := os.ReadFile(configPath); err != nil || string(body) != string(original) {
		t.Fatal("dry-run changed config")
	}
	applied, err := fix.Apply(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Changed || !applied.Verified {
		t.Fatalf("applied = %#v", applied)
	}
	if attempts != 3 || waits != 2 {
		t.Fatalf("verification attempts=%d waits=%d", attempts, waits)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "example-api-key") {
		t.Fatal("template key remains after repair")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(key)) == "" || strings.Contains(applied.Summary, strings.TrimSpace(string(key))) {
		t.Fatal("repair did not persist a private key or disclosed it")
	}
	if err := fix.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil || string(body) != string(original) {
		t.Fatal("rollback did not restore exact config bytes")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("rollback retained newly created key file: %v", err)
	}
}

func TestSafeModeFixPatchesAdoptedConfigAndRefreshesKeyReference(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	original := []byte("host: 127.0.0.1\nport: 8317\nauth-dir: " + filepath.Join(root, "auth") + "\napi-keys:\n  - example-api-key\nws-auth: true\nremote-management:\n  allow-remote: false\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	platform, err := adapterplatform.New(filepath.Join(root, "config-root"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(state.Paths{Config: filepath.Join(root, "config-root", "config.json"), State: filepath.Join(root, "state", "state.json"), Secrets: filepath.Join(root, "state", "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	installation := state.Installation{
		ID: "default", Kind: "adopted", BinaryPath: filepath.Join(root, "cli-proxy-api"), ConfigPath: configPath,
		ProxyKeyRef: state.SecretReference{Path: configPath, Masked: "********", Fingerprint: fingerprintOf("example-api-key")},
		AuthDir:     filepath.Join(root, "auth"), RuntimeDir: filepath.Join(root, "runtime"), Host: "127.0.0.1", Port: 8317, ServiceBackend: "foreground",
	}
	current, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	current.Installations = []state.Installation{installation}
	if err := store.SaveState(current); err != nil {
		t.Fatal(err)
	}
	fix := &safeModeFix{
		adapter: configfile.New(filepath.Join(root, "backups")), installation: installation, platform: platform, store: store,
		verify: func(context.Context, string) error { return nil },
	}
	result, err := fix.Apply(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified {
		t.Fatalf("result = %#v", result)
	}
	snapshot, err := fix.adapter.Read(context.Background(), configPath)
	if err != nil {
		t.Fatalf("adopted config was clobbered rather than patched: %v", err)
	}
	if snapshot.Config.Host != "127.0.0.1" || len(snapshot.Config.APIKeys) != 1 || configfile.IsTemplateAPIKey(snapshot.Config.APIKeys[0]) {
		t.Fatalf("patched adopted config = %#v", snapshot.Config)
	}
	saved, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Installations) != 1 || saved.Installations[0].ProxyKeyRef.Path != configPath || saved.Installations[0].ProxyKeyRef.Fingerprint != fingerprintOf(snapshot.Config.APIKeys[0]) {
		t.Fatalf("refreshed installation = %#v", saved.Installations)
	}
}
