package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStoreAtomicRoundTripAndPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	store, err := New(Paths{
		Config: filepath.Join(root, "config", "config.json"),
		State: filepath.Join(root, "state", "state.json"),
		Secrets: filepath.Join(root, "state", "secrets.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Theme: "high-contrast", UpdateCheck: true, LogLineLimit: 250}
	if err := store.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	config.Theme = "plain"
	if err := store.SaveConfig(config); err != nil {
		t.Fatalf("atomic replacement failed: %v", err)
	}
	gotConfig, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if gotConfig.Version != SchemaVersion || gotConfig.Theme != config.Theme || !gotConfig.UpdateCheck {
		t.Fatalf("unexpected config: %#v", gotConfig)
	}

	reference := SecretReference{Path: filepath.Join(root, "private", "api-key.txt"), Masked: "sk-ab12…9xyz", Fingerprint: "sha256:" + strings.Repeat("a", 64)}
	value := State{Installations: []Installation{{
		ID: "default", Kind: "managed", BinaryPath: filepath.Join(root, "bin", "cli-proxy-api"),
		ConfigPath: filepath.Join(root, "instance", "config.yaml"), ProxyKeyRef: reference,
		AuthDir: filepath.Join(root, "instance", "auth"), RuntimeDir: filepath.Join(root, "instance", "runtime"),
		Host: "127.0.0.1", Port: 8317, ServiceBackend: "foreground",
	}}}
	if err := store.SaveState(value); err != nil {
		t.Fatal(err)
	}
	gotState, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotState.Installations) != 1 || gotState.Installations[0].ProxyKeyRef != reference {
		t.Fatalf("unexpected state: %#v", gotState)
	}

	secretRefs := SecretReferences{Management: map[string]SecretReference{"default": {
		Path: filepath.Join(root, "private", "management.secret"), Masked: "abcdefg…wxyz", Fingerprint: "sha256:" + strings.Repeat("b", 64),
	}}}
	if err := store.SaveSecretReferences(secretRefs); err != nil {
		t.Fatal(err)
	}
	gotRefs, err := store.LoadSecretReferences()
	if err != nil {
		t.Fatal(err)
	}
	if gotRefs.Management["default"] != secretRefs.Management["default"] {
		t.Fatalf("unexpected references: %#v", gotRefs)
	}

	for _, path := range []string{store.paths.Config, store.paths.State, store.paths.Secrets} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, info.Mode().Perm())
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".pmux-json-") {
				t.Fatalf("temporary file remains after commit: %s", entry.Name())
			}
		}
	}
}

func TestStatePersistsReferencesButNotSecretValues(t *testing.T) {
	root := t.TempDir()
	store, err := New(Paths{Config: filepath.Join(root, "config.json"), State: filepath.Join(root, "state.json"), Secrets: filepath.Join(root, "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	canary := "sk-CANARY-SHOULD-NEVER-BE-PERSISTED"
	ref := SecretReference{Path: filepath.Join(root, "canonical-secret-source"), Masked: "sk-CANA…STED", Fingerprint: "sha256:" + strings.Repeat("c", 64)}
	unsafeReference := ref
	unsafeReference.Masked = canary
	if err := store.SaveSecretReferences(SecretReferences{Management: map[string]SecretReference{"default": unsafeReference}}); err == nil {
		t.Fatal("expected plaintext secret-shaped mask to be rejected")
	}
	if err := store.SaveSecretReferences(SecretReferences{Management: map[string]SecretReference{"default": ref}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveState(State{Installations: []Installation{{
		ID: "default", Kind: "managed", BinaryPath: filepath.Join(root, "proxy"), ConfigPath: filepath.Join(root, "config.yaml"),
		ProxyKeyRef: ref, AuthDir: filepath.Join(root, "auth"), RuntimeDir: filepath.Join(root, "runtime"), Host: "127.0.0.1", Port: 8317,
	}}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.paths.State, store.paths.Secrets} {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), canary) {
			t.Fatalf("secret canary persisted in %s", path)
		}
		var document map[string]any
		if err := json.Unmarshal(payload, &document); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(Paths{Config: filepath.Join(root, "config.json"), State: path, Secrets: filepath.Join(root, "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadState(); err == nil {
		t.Fatal("expected unsupported newer schema error")
	}
}
