package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/state"
)

func TestLoadAdoptedProxyKeyParsesConfigAndMatchesFingerprint(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	first, selected := "sk-first-1234567890", "sk-selected-0987654321"
	body := "host: 127.0.0.1\nport: 8317\nauth-dir: " + filepath.Join(root, "auth") + "\napi-keys:\n  - " + first + "\n  - " + selected + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	installation := state.Installation{ID: "adopted", ConfigPath: configPath, ProxyKeyRef: state.SecretReference{Path: configPath, Fingerprint: fingerprintOf(selected)}}
	key, err := (&nativeRuntime{roots: domainplatform.Roots{State: root}}).loadProxyKey(context.Background(), installation)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if string(key) != selected {
		t.Fatalf("selected key = %q", key)
	}
	installation.ProxyKeyRef.Fingerprint = fingerprintOf("missing")
	if _, err := (&nativeRuntime{roots: domainplatform.Roots{State: root}}).loadProxyKey(context.Background(), installation); err == nil {
		t.Fatal("mismatched adopted key fingerprint was accepted")
	}
}

func TestManagedBinaryFactVerifiesRecordedSHA256(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cli-proxy-api")
	body := []byte("managed binary")
	if err := os.WriteFile(binary, body, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	installation := state.Installation{Kind: "managed", BinaryPath: binary, BinarySHA256: "sha256:" + hex.EncodeToString(digest[:])}
	fact, err := (&doctorSource{installation: installation}).Binary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !fact.ChecksumOK {
		t.Fatalf("checksum fact = %#v", fact)
	}
	if err := os.WriteFile(binary, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	fact, err = (&doctorSource{installation: installation}).Binary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fact.ChecksumOK {
		t.Fatal("tampered managed binary passed checksum verification")
	}
}

func TestAbsoluteConfigFactUsesEffectiveServiceDefinition(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	installation := state.Installation{
		ID: "default", ServiceBackend: string(service.BackendSystemdUser),
		ConfigPath: filepath.Join(root, "config.yaml"), RuntimeDir: filepath.Join(root, "runtime"),
	}
	unitDir := filepath.Join(root, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := "[Service]\nWorkingDirectory=" + installation.RuntimeDir + "\nExecStart=/tmp/cli-proxy-api --config " + installation.ConfigPath + "\nEnvironment=PGSTORE_HOST=attacker\n"
	if err := os.WriteFile(filepath.Join(unitDir, service.Identity(service.BackendSystemdUser, installation.ID)), []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &doctorSource{runtime: &nativeRuntime{roots: domainplatform.Roots{State: filepath.Join(root, "state")}}, installation: installation}
	fact, err := source.AbsoluteConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !fact.ArgvUsesAbsolutePath || fact.RuntimeDir != installation.RuntimeDir || !reflect.DeepEqual(fact.StoreOverrides, []string{"PGSTORE_HOST"}) {
		t.Fatalf("effective definition fact = %#v", fact)
	}
}
