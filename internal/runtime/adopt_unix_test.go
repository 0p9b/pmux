//go:build !windows

package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/state"
)

func TestAdoptHardeningCreatesPrivateManagementReference(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-root"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache-root"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data-root"))
	platform, err := adapterplatform.New(filepath.Join(root, "config-root"))
	if err != nil {
		t.Fatal(err)
	}
	roots := testRoots(root)
	store, err := state.New(state.Paths{Config: filepath.Join(roots.Config, "config.json"), State: filepath.Join(roots.State, "state.json"), Secrets: filepath.Join(roots.State, "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "cli-proxy-api")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("X-CPA-VERSION", "7.2.92")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case "/v0/management/auth-files":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	serverPort, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	config := "host: 127.0.0.1\nport: " + strconv.Itoa(serverPort) + "\nauth-dir: " + authDir + "\napi-keys:\n  - example-api-key\nremote-management:\n  allow-remote: true\nws-auth: false\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeService := &hardeningService{}
	native := &nativeRuntime{platform: platform, roots: roots, store: store, http: server.Client(), serviceFactory: func(context.Context, state.Installation, bool) (service.ServiceManager, error) {
		return fakeService, nil
	}}
	out, err := native.adopt(context.Background(), app.SetupRequest{Mode: "adopt", ProxyPath: binary, ConfigPath: configPath, Harden: true, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Hardened {
		t.Fatal("adoption did not report hardening")
	}
	if fakeService.restarts != 1 || fakeService.installed.ConfigPath != configPath || fakeService.installed.RuntimeDir != out.Installation.RuntimeDir {
		t.Fatalf("service hardening = restarts %d spec %#v", fakeService.restarts, fakeService.installed)
	}
	if out.Installation.ProxyKeyRef.Path == configPath {
		t.Fatalf("hardened proxy key still references config YAML: %#v", out.Installation.ProxyKeyRef)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "example-api-key") || !strings.Contains(string(body), "secret-key:") || !strings.Contains(string(body), "ws-auth: true") {
		t.Fatalf("hardened config is missing required security changes: %s", body)
	}
	refs, err := store.LoadSecretReferences()
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := refs.Management["default"]
	if !ok || ref.Path == "" || ref.Masked == "" || !strings.HasPrefix(ref.Fingerprint, "sha256:") {
		t.Fatalf("management reference = %#v", ref)
	}
	secret, err := os.ReadFile(ref.Path)
	if err != nil || strings.TrimSpace(string(secret)) == "" {
		t.Fatalf("private management secret missing: %v", err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(roots.State, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), strings.TrimSpace(string(secret))) {
		t.Fatal("state.json contains the management secret")
	}
}

func TestAdoptHardeningRollbackRestoresConfigAndAdoptionRecord(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	platform, err := adapterplatform.New(filepath.Join(root, "config-root"))
	if err != nil {
		t.Fatal(err)
	}
	roots := testRoots(root)
	store, err := state.New(state.Paths{Config: filepath.Join(roots.Config, "config.json"), State: filepath.Join(roots.State, "state.json"), Secrets: filepath.Join(roots.State, "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "cli-proxy-api")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	original := []byte("host: 127.0.0.1\nport: 8317\nauth-dir: " + filepath.Join(root, "auth") + "\napi-keys:\n  - example-api-key\nremote-management:\n  allow-remote: true\nws-auth: false\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeService := &hardeningService{restartErr: errors.New("injected restart failure")}
	native := &nativeRuntime{platform: platform, roots: roots, store: store, serviceFactory: func(context.Context, state.Installation, bool) (service.ServiceManager, error) {
		return fakeService, nil
	}}
	if _, err := native.adopt(context.Background(), app.SetupRequest{Mode: "adopt", ProxyPath: binary, ConfigPath: configPath, Harden: true, Yes: true}); err == nil {
		t.Fatal("hardening unexpectedly succeeded")
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("config was not rolled back:\n%s", restored)
	}
	current, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Installations) != 1 || current.Installations[0].Kind != "adopted" || current.Installations[0].ProxyKeyRef.Path != configPath {
		t.Fatalf("adoption record was not retained: %#v", current.Installations)
	}
	refs, err := store.LoadSecretReferences()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Management) != 0 {
		t.Fatalf("management reference survived rollback: %#v", refs.Management)
	}
}
