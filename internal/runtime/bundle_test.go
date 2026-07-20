package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/state"
)

type bundleService struct {
	hardeningService
	log string
}

func (s *bundleService) Logs(context.Context, int, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.log)), nil
}

func TestRuntimeBundleIncludesBoundedEvidenceAndRedactsCanaries(t *testing.T) {
	root := t.TempDir()
	roots := domainplatform.Roots{
		Config: filepath.Join(root, "config-root"), State: filepath.Join(root, "state-root"),
		Cache: filepath.Join(root, "cache-root"), Data: filepath.Join(root, "data-root"),
	}
	platform, err := adapterplatform.New(roots.Config)
	if err != nil { t.Fatal(err) }
	store, err := state.New(state.Paths{
		Config: filepath.Join(roots.Config, "config.json"), State: filepath.Join(roots.State, "state.json"),
		Secrets: filepath.Join(roots.State, "secrets.json"),
	})
	if err != nil { t.Fatal(err) }

	proxyCanary := "sk-proxy-canary-abcdefghijklmnopqrstuvwxyz"
	managementCanary := "management-canary-abcdefghijklmnopqrstuvwxyz"
	authCanary := "AUTH-FILE-CONTENT-MUST-NOT-APPEAR"
	instanceDir := filepath.Join(roots.Data, "instances", "default")
	authDir := filepath.Join(instanceDir, "auth")
	runtimeDir := filepath.Join(instanceDir, "runtime")
	if err := os.MkdirAll(authDir, 0o700); err != nil { t.Fatal(err) }
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil { t.Fatal(err) }
	configPath := filepath.Join(instanceDir, "config.yaml")
	config := "host: 127.0.0.1\nport: 8317\nauth-dir: " + authDir + "\napi-keys:\n  - " + proxyCanary + "\nws-auth: true\nremote-management:\n  allow-remote: false\n  disable-control-panel: true\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(authDir, "account.json"), []byte(authCanary), 0o600); err != nil { t.Fatal(err) }
	managementPath := filepath.Join(roots.State, "management.secret")
	if err := os.MkdirAll(roots.State, 0o700); err != nil { t.Fatal(err) }
	if err := os.WriteFile(managementPath, []byte(managementCanary+"\n"), 0o600); err != nil { t.Fatal(err) }
	if err := store.SaveSecretReferences(state.SecretReferences{Management: map[string]state.SecretReference{
		"default": secretReference(managementPath, managementCanary),
	}}); err != nil { t.Fatal(err) }
	for path, body := range map[string]string{
		filepath.Join(roots.State, "logs", "pmux.log"): "token=" + proxyCanary + "\n",
		filepath.Join(roots.State, "audit.jsonl"):       `{"management":"` + managementCanary + `"}` + "\n",
		filepath.Join(roots.State, "journal.jsonl"):     `{"authorization":"Bearer ` + proxyCanary + `"}` + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { t.Fatal(err) }
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil { t.Fatal(err) }
	}
	binaryPath := filepath.Join(root, "cli-proxy-api")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0o700); err != nil { t.Fatal(err) }
	installation := state.Installation{
		ID: "default", Kind: "managed", BinaryPath: binaryPath, ConfigPath: configPath,
		ProxyKeyRef: secretReference(configPath, proxyCanary), AuthDir: authDir, RuntimeDir: runtimeDir,
		Host: "127.0.0.1", Port: 8317, ServiceBackend: string(service.BackendForeground), CoreVersionSeen: "7.2.92",
	}
	current, err := store.LoadState()
	if err != nil { t.Fatal(err) }
	current.Installations = []state.Installation{installation}
	if err := store.SaveState(current); err != nil { t.Fatal(err) }
	fakeService := &bundleService{log: "service token " + proxyCanary + "\n"}
	native := &nativeRuntime{
		platform: platform, roots: roots, store: store,
		serviceFactory: func(context.Context, state.Installation, bool) (service.ServiceManager, error) { return fakeService, nil },
	}
	destination := filepath.Join(root, "doctor.zip")
	if _, err := native.Bundle(context.Background(), installation, destination); err != nil { t.Fatal(err) }

	reader, err := zip.OpenReader(destination)
	if err != nil { t.Fatal(err) }
	defer reader.Close()
	wanted := map[string]bool{"state.json": false, "doctor.json": false, "config-summary.json": false, "service.json": false, "logs/service.log": false, "logs/pmux.log": false, "audit.jsonl": false, "journal.jsonl": false}
	var archive bytes.Buffer
	for _, file := range reader.File {
		if _, ok := wanted[file.Name]; ok { wanted[file.Name] = true }
		handle, err := file.Open()
		if err != nil { t.Fatal(err) }
		if _, err := io.Copy(&archive, handle); err != nil { handle.Close(); t.Fatal(err) }
		if err := handle.Close(); err != nil { t.Fatal(err) }
	}
	for name, present := range wanted {
		if !present { t.Errorf("bundle entry %q is missing", name) }
	}
	for _, canary := range []string{proxyCanary, managementCanary, authCanary} {
		if bytes.Contains(archive.Bytes(), []byte(canary)) { t.Fatalf("bundle disclosed canary %q", canary) }
	}
	if bytes.Contains(archive.Bytes(), []byte("account.json")) { t.Fatal("bundle disclosed an auth-file name") }
}
