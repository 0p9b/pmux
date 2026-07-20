package runtime

import (
	"context"
	"errors"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

func TestNativeModelTesterRequiresLiveExactModelAndUsesLocalProxy(t *testing.T) {
	const exactID = "provider/model:exact"
	var testedModel string
	offline := false
	completionCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline && (r.URL.Path == "/v0/management/auth-files" || r.URL.Path == "/v1/models") {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/v0/management/auth-files":
			_, _ = io.WriteString(w, `[{"name":"codex-user.json","provider":"codex","status":"ready"}]`)
		case "/v0/management/auth-files/models":
			_, _ = io.WriteString(w, `{"models":[{"id":"`+exactID+`","channel":"codex","available":true}]}`)
		case "/v0/management/model-definitions/codex":
			_, _ = io.WriteString(w, `{"models":[{"id":"`+exactID+`"}]}`)
		case "/v1/chat/completions":
			completionCalls++
			var body struct {
				Model string `json:"model"`
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			testedModel = body.Model
			if body.MaxTokens != 1 {
				t.Errorf("max_tokens = %d", body.MaxTokens)
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state-root"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache-root"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data-root"))
	platform, err := adapterplatform.New(filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	roots, err := loadRoots(platform)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(state.Paths{Config: filepath.Join(roots.Config, "config.json"), State: filepath.Join(roots.State, "state.json"), Secrets: filepath.Join(roots.State, "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(root, "proxy.key")
	managementPath := filepath.Join(root, "management.key")
	if err := os.WriteFile(proxyPath, []byte("sk-test-1234567890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managementPath, []byte("management-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installation := state.Installation{ID: "default", Kind: "managed", BinaryPath: filepath.Join(root, "cli-proxy-api"), ConfigPath: filepath.Join(root, "config.yaml"), ProxyKeyRef: secretReference(proxyPath, "sk-test-1234567890"), AuthDir: filepath.Join(root, "auth"), RuntimeDir: filepath.Join(root, "runtime"), Host: "127.0.0.1", Port: port, ServiceBackend: "foreground"}
	current, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	current.Installations = []state.Installation{installation}
	if err := store.SaveState(current); err != nil {
		t.Fatal(err)
	}
	refs, err := store.LoadSecretReferences()
	if err != nil {
		t.Fatal(err)
	}
	refs.Management[installation.ID] = secretReference(managementPath, "management-test")
	if err := store.SaveSecretReferences(refs); err != nil {
		t.Fatal(err)
	}
	native := &nativeRuntime{platform: platform, roots: roots, store: store, http: server.Client()}
	result, err := native.Test(context.Background(), installation, exactID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if testedModel != exactID {
		t.Fatalf("tested model = %q", testedModel)
	}
	if result.(map[string]any)["model"] != exactID {
		t.Fatalf("result = %#v", result)
	}
	if completionCalls != 1 {
		t.Fatalf("completion calls = %d, want 1", completionCalls)
	}
	offline = true
	if _, err := native.Test(context.Background(), installation, exactID, time.Second); err == nil {
		t.Fatal("model tester accepted a stale cached model while live discovery was offline")
	} else {
		var typed *pmuxerr.Error
		if !errors.As(err, &typed) || typed.Code != pmuxerr.ManagementUnreachable {
			t.Fatalf("stale model error = %#v, want %s", err, pmuxerr.ManagementUnreachable)
		}
	}
	if completionCalls != 1 {
		t.Fatalf("stale model caused billable request; completion calls = %d", completionCalls)
	}
	if _, err := native.Test(context.Background(), installation, "provider/model:other", time.Second); err == nil {
		t.Fatal("model tester accepted an ID absent from the live catalog")
	}
}

