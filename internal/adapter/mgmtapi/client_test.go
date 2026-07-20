package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	testManagementKey = "management-secret-canary-value"
	testProxyKey      = "sk-proxy-secret-canary-value"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := New(Options{BaseURL: server.URL, ManagementKey: testManagementKey, ProxyKey: testProxyKey})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return client, server
}

func TestClientImplementsEveryConsumedMethod(t *testing.T) {
	var mu sync.Mutex
	configYAML := []byte("host: 127.0.0.1\n")
	seen := map[string]int{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.EscapedPath()]++
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, managementPrefix) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+testManagementKey {
				t.Errorf("management authorization = %q", got)
			}
		} else if r.URL.Path == "/v1/models" {
			if got := r.Header.Get("Authorization"); got != "Bearer "+testProxyKey {
				t.Errorf("proxy authorization = %q", got)
			}
		} else if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("health authorization unexpectedly set: %q", got)
		}

		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("X-CPA-VERSION", "7.2.92")
			writeJSON(t, w, map[string]string{"status": "ok"})
		case r.URL.Path == "/v1/models":
			writeJSON(t, w, map[string]any{"data": []map[string]any{{"id": "runtime/model", "owned_by": "vendor"}}})
		case r.URL.Path == managementPrefix+"/config" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"port": 8317, "api-keys": []string{testProxyKey}})
		case r.URL.Path == managementPrefix+"/config.yaml" && r.Method == http.MethodGet:
			_, _ = w.Write(configYAML)
		case r.URL.Path == managementPrefix+"/config.yaml" && r.Method == http.MethodPut:
			configYAML, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == managementPrefix+"/api-keys" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"api-keys": []string{testProxyKey}})
		case r.URL.Path == managementPrefix+"/api-key-usage" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"usage": []management.APIKeyUsage{{Fingerprint: "sha256:x", Requests: 2}}})
		case isProviderPath(r.URL.Path) && r.Method == http.MethodGet:
			writeJSON(t, w, []management.ProviderKey{{ID: "one", Fields: map[string]string{"api_key": testProxyKey}}})
		case r.URL.Path == managementPrefix+"/auth-files" && r.Method == http.MethodGet:
			writeJSON(t, w, []management.AuthFile{{Name: "codex-user.json", Provider: "codex", Status: "ready"}})
		case r.URL.Path == managementPrefix+"/auth-files/models":
			writeJSON(t, w, map[string]any{"models": []management.ModelRef{{ID: "runtime/model", Channel: "codex", Available: true}}})
		case strings.HasPrefix(r.URL.Path, managementPrefix+"/model-definitions/"):
			writeJSON(t, w, map[string]any{"models": []management.ModelDef{{ID: "runtime/model"}}})
		case r.URL.Path == managementPrefix+"/oauth-excluded-models" && r.Method == http.MethodGet:
			writeJSON(t, w, management.ExcludedModelSet{"codex": {"runtime/model"}})
		case r.URL.Path == managementPrefix+"/oauth-model-alias" && r.Method == http.MethodGet:
			writeJSON(t, w, management.ModelAliasSet{"codex": {"alias": "runtime/model"}})
		case strings.HasSuffix(r.URL.Path, "-auth-url"):
			writeJSON(t, w, management.OAuthChallenge{State: "state", URL: "https://login.example/authorize", UserCode: "ABCD"})
		case r.URL.Path == managementPrefix+"/get-auth-status":
			writeJSON(t, w, management.OAuthStatus{State: r.URL.Query().Get("state"), Status: "pending"})
		case r.URL.Path == managementPrefix+"/logs" && r.Method == http.MethodGet:
			writeJSON(t, w, management.LogPage{Records: []management.LogRecord{{Level: "info", Message: "Bearer " + testProxyKey}}})
		case r.URL.Path == managementPrefix+"/request-error-logs" && r.Method == http.MethodGet:
			writeJSON(t, w, []management.RequestErrorLog{{Name: "one.log", Message: "safe"}})
		case strings.HasPrefix(r.URL.Path, managementPrefix+"/request-error-logs/"):
			writeJSON(t, w, management.RequestErrorLog{Name: "one.log", Message: "safe"})
		case strings.HasPrefix(r.URL.Path, managementPrefix+"/request-log-by-id/"):
			writeJSON(t, w, management.RequestLog{ID: "request", Status: 200, Method: "GET", Path: "/v1/models"})
		case r.URL.Path == managementPrefix+"/usage-queue":
			writeJSON(t, w, []management.UsageRecord{{Model: "runtime/model", Status: 200}})
		case r.URL.Path == managementPrefix+"/vertex/import":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
				t.Errorf("vertex content type = %q", r.Header.Get("Content-Type"))
			}
			writeJSON(t, w, management.VertexImportResult{Name: "vertex-project.json"})
		case r.URL.Path == managementPrefix+"/latest-version":
			writeJSON(t, w, map[string]string{"version": "7.2.93"})
		case r.URL.Path == managementPrefix+"/api-call":
			writeJSON(t, w, map[string]any{"status": 200, "headers": map[string][]string{"Authorization": {"Bearer " + testProxyKey}}, "body": "response " + testProxyKey})
		default:
			// All remaining consumed routes are mutations, scalar reads, or log deletion.
			if r.Method == http.MethodGet {
				writeJSON(t, w, map[string]any{"value": true})
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		}
	}
	client, _ := newTestClient(t, handler)
	ctx := context.Background()

	if info, err := client.Health(ctx); err != nil || !info.Healthy || info.Version != "7.2.92" {
		t.Fatalf("Health: %#v, %v", info, err)
	}
	if models, err := client.PublicModels(ctx); err != nil || len(models) != 1 {
		t.Fatalf("PublicModels: %#v, %v", models, err)
	}
	if config, err := client.Config(ctx); err != nil || strings.Contains(string(mustJSON(t, config)), testProxyKey) {
		t.Fatalf("Config redaction: %#v, %v", config, err)
	}
	if _, err := client.ConfigYAML(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.PutConfigYAML(ctx, []byte("port: 9000\n")); err != nil {
		t.Fatal(err)
	}

	setting := management.SettingName("debug")
	if _, err := client.GetSetting(ctx, setting); err != nil {
		t.Fatal(err)
	}
	if err := client.PutSetting(ctx, setting, management.SettingValue(`true`)); err != nil {
		t.Fatal(err)
	}
	if err := client.PatchSetting(ctx, setting, management.SettingPatch(`{"value":false}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSetting(ctx, management.SettingName("proxy-url")); err != nil {
		t.Fatal(err)
	}

	keys, err := client.APIKeys(ctx)
	if err != nil || len(keys) != 1 || strings.Contains(keys[0].Mask, testProxyKey) {
		t.Fatalf("APIKeys: %#v, %v", keys, err)
	}
	if err := client.PutAPIKeys(ctx, []management.SecretValue{"sk-new-secret-value"}); err != nil {
		t.Fatal(err)
	}
	if err := client.PatchAPIKeys(ctx, management.KeyPatch(`{"old":"x","new":"y"}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteAPIKey(ctx, keys[0].Fingerprint); err != nil {
		t.Fatal(err)
	}
	if usage, err := client.APIKeyUsage(ctx); err != nil || len(usage) != 1 {
		t.Fatalf("APIKeyUsage: %#v, %v", usage, err)
	}

	for _, kind := range []management.ProviderKeyKind{management.ProviderGemini, management.ProviderInteractions, management.ProviderClaude, management.ProviderCodex, management.ProviderXAI, management.ProviderVertex, management.ProviderOpenAICompatible} {
		values, err := client.ProviderKeys(ctx, kind)
		if err != nil || len(values) != 1 || values[0].Fields["api_key"] == testProxyKey {
			t.Fatalf("ProviderKeys(%s): %#v, %v", kind, values, err)
		}
		if err := client.PutProviderKeys(ctx, kind, []management.ProviderKey{{ID: "two"}}); err != nil {
			t.Fatal(err)
		}
		if err := client.PatchProviderKeys(ctx, kind, management.ProviderKeyPatch(`{"id":"two"}`)); err != nil {
			t.Fatal(err)
		}
		if err := client.DeleteProviderKey(ctx, kind, "two"); err != nil {
			t.Fatal(err)
		}
	}

	if values, err := client.AuthFiles(ctx); err != nil || len(values) != 1 {
		t.Fatalf("AuthFiles: %#v, %v", values, err)
	}
	if values, err := client.AuthFileModels(ctx, "codex-user.json"); err != nil || len(values) != 1 {
		t.Fatalf("AuthFileModels: %#v, %v", values, err)
	}
	if values, err := client.ModelDefinitions(ctx, "codex"); err != nil || len(values) != 1 {
		t.Fatalf("ModelDefinitions: %#v, %v", values, err)
	}
	if err := client.DeleteAuthFiles(ctx, []string{"codex-user.json"}, false); err != nil {
		t.Fatal(err)
	}
	if err := client.PatchAuthFileStatus(ctx, management.AuthFileStatusPatch(`{"name":"codex-user.json"}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.PatchAuthFileFields(ctx, management.AuthFileFieldsPatch(`{"name":"codex-user.json"}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := client.ExcludedModels(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.PutExcludedModels(ctx, management.ExcludedModelSet{}); err != nil {
		t.Fatal(err)
	}
	if err := client.PatchExcludedModels(ctx, management.ExcludedModelPatch(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteExcludedModels(ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ModelAliases(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.PutModelAliases(ctx, management.ModelAliasSet{}); err != nil {
		t.Fatal(err)
	}
	if err := client.PatchModelAliases(ctx, management.ModelAliasPatch(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteModelAliases(ctx, "codex"); err != nil {
		t.Fatal(err)
	}

	for _, provider := range []management.ProviderID{"claude", "codex", "antigravity", "kimi", "xai"} {
		if _, err := client.BeginOAuth(ctx, provider, true); err != nil {
			t.Fatalf("BeginOAuth(%s): %v", provider, err)
		}
	}
	if _, err := client.OAuthStatus(ctx, "state"); err != nil {
		t.Fatal(err)
	}
	if err := client.SubmitOAuthCallback(ctx, "http://127.0.0.1/callback?code=secret&state=state"); err != nil {
		t.Fatal(err)
	}
	if err := client.CancelOAuth(ctx, "state"); err != nil {
		t.Fatal(err)
	}

	logs, err := client.Logs(ctx, management.LogQuery{Level: "info", Since: time.Unix(1, 0), Tail: 5})
	if err != nil || strings.Contains(logs.Records[0].Message, testProxyKey) {
		t.Fatalf("Logs: %#v, %v", logs, err)
	}
	if err := client.DeleteLogs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestErrorLogs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestErrorLog(ctx, "name/with +?#"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestLogByID(ctx, "id/with +?#"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PopUsageQueue(ctx); err != nil {
		t.Fatal(err)
	}

	serviceAccount := t.TempDir() + "/vertex.json"
	if err := os.WriteFile(serviceAccount, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ImportVertex(ctx, management.VertexImportRequest{Path: serviceAccount, Prefix: "team"}); err != nil {
		t.Fatal(err)
	}
	if err := client.ResetQuota(ctx, management.ResetQuotaRequest{Name: "codex-user.json"}); err != nil {
		t.Fatal(err)
	}
	if version, err := client.LatestVersion(ctx); err != nil || version != "7.2.93" {
		t.Fatalf("LatestVersion: %q, %v", version, err)
	}
	apiResponse, err := client.APICall(ctx, management.APICallRequest{Method: "GET", URL: "https://api.example/models"})
	if err != nil || strings.Contains(string(apiResponse.Body), testProxyKey) || apiResponse.Headers["Authorization"][0] != "********" {
		t.Fatalf("APICall redaction: %#v, %v", apiResponse, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 45 {
		t.Fatalf("only %d distinct method/path contracts exercised: %#v", len(seen), seen)
	}
}

func TestCapabilitiesProbeEndpointsIndependentlyOfVersion(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("X-CPA-VERSION", "0.0.1")
			writeJSON(t, w, map[string]string{"status": "ok"})
			return
		}
		if r.URL.Path == "/v1/models" {
			writeJSON(t, w, map[string]any{"data": []any{}})
			return
		}
		if r.URL.Path == managementPrefix+"/config" {
			writeJSON(t, w, map[string]any{})
			return
		}
		if r.URL.Path == managementPrefix+"/api-call" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(t, w, map[string]any{})
	})
	capabilities, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities["management"] || !capabilities["management-config-yaml"] {
		t.Fatalf("expected probed capabilities: %#v", capabilities)
	}
	if capabilities["management-api-call"] {
		t.Fatalf("missing endpoint reported available: %#v", capabilities)
	}
}

func TestManagementAuthIsBearerAndNeverRetried(t *testing.T) {
	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Header.Get("Authorization") != "Bearer "+testManagementKey {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"` + testManagementKey + `"}`))
	})
	_, err := client.Config(context.Background())
	assertPMuxCode(t, err, pmuxerr.ManagementAuthRejected)
	if attempts != 1 {
		t.Fatalf("auth attempts = %d, want exactly 1", attempts)
	}
	if strings.Contains(err.Error(), testManagementKey) {
		t.Fatal("management secret leaked in error")
	}
}

func TestManagement404Taxonomy(t *testing.T) {
	t.Run("individual endpoint absent", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == managementPrefix+"/config" {
				writeJSON(t, w, map[string]any{})
				return
			}
			http.NotFound(w, r)
		})
		_, err := client.LatestVersion(context.Background())
		assertPMuxCode(t, err, pmuxerr.UnhandledUpstreamShape)
		if !strings.Contains(err.Error(), "individual") && !strings.Contains(err.Error(), "requested") {
			t.Fatalf("endpoint absence not explained: %v", err)
		}
	})
	t.Run("management disabled", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
		_, err := client.LatestVersion(context.Background())
		assertPMuxCode(t, err, pmuxerr.ManagementUnreachable)
		if !strings.Contains(strings.ToLower(err.Error()), "disabled") {
			t.Fatalf("disabled management not explained: %v", err)
		}
	})
	t.Run("ban guidance", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
		_, err := client.Config(context.Background())
		assertPMuxCode(t, err, pmuxerr.ManagementAuthRejected)
		if !strings.Contains(err.Error(), "30 minutes") {
			t.Fatalf("ban window absent: %v", err)
		}
	})
}

func TestResponseBoundAndTimeout(t *testing.T) {
	t.Run("bound", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 33))) }))
		defer server.Close()
		client, err := New(Options{BaseURL: server.URL, ManagementKey: testManagementKey, MaxResponseSize: 32})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Config(context.Background())
		assertPMuxCode(t, err, pmuxerr.UnhandledUpstreamShape)
	})
	t.Run("default local timeout", func(t *testing.T) {
		client, _ := New(Options{BaseURL: "http://127.0.0.1:8317", ManagementKey: testManagementKey})
		if client.timeout != 2*time.Second {
			t.Fatalf("timeout = %s", client.timeout)
		}
	})
	t.Run("deadline taxonomy", func(t *testing.T) {
		client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			writeJSON(t, w, map[string]any{})
		})
		_ = server
		client.timeout = 5 * time.Millisecond
		_, err := client.Config(context.Background())
		assertPMuxCode(t, err, pmuxerr.ManagementUnreachable)
	})
}

func TestCentralPathAndQueryEncoding(t *testing.T) {
	var escapedPath, modelName string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		escapedPath = r.URL.EscapedPath()
		modelName = r.URL.Query().Get("name")
		writeJSON(t, w, map[string]any{"models": []any{}})
	})
	if _, err := client.ModelDefinitions(context.Background(), "channel/with +?#"); err != nil {
		t.Fatal(err)
	}
	if escapedPath != managementPrefix+"/model-definitions/channel%2Fwith%20+%3F%23" {
		t.Fatalf("escaped path = %q", escapedPath)
	}
	if _, err := client.AuthFileModels(context.Background(), "file/with +&?.json"); err != nil {
		t.Fatal(err)
	}
	if modelName != "file/with +&?.json" {
		t.Fatalf("decoded query = %q", modelName)
	}
}

func TestUpstreamWireShapes(t *testing.T) {
	var scalarBody, keysBody []byte
	var deleteIndex, logsAfter string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == managementPrefix+"/debug" && r.Method == http.MethodPut:
			scalarBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == managementPrefix+"/api-keys" && r.Method == http.MethodPut:
			keysBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == managementPrefix+"/api-keys" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"api-keys": []string{"sk-first-secret", "sk-second-secret"}})
		case r.URL.Path == managementPrefix+"/api-keys" && r.Method == http.MethodDelete:
			deleteIndex = r.URL.Query().Get("index")
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == managementPrefix+"/api-key-usage":
			writeJSON(t, w, map[string]any{"codex": map[string]any{"https://example.test|sk-usage-secret": map[string]int64{"success": 4, "failed": 1}}})
		case r.URL.Path == managementPrefix+"/logs":
			logsAfter = r.URL.Query().Get("after")
			writeJSON(t, w, map[string]any{"lines": []string{"Bearer " + testProxyKey}, "next-cursor": "cursor-2"})
		case r.URL.Path == managementPrefix+"/request-error-logs":
			writeJSON(t, w, map[string]any{"files": []management.RequestErrorLog{{Name: "request.log"}}})
		case r.URL.Path == managementPrefix+"/latest-version":
			writeJSON(t, w, map[string]string{"latest-version": "7.2.93"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	if err := client.PutSetting(ctx, management.SettingName("debug"), management.SettingValue(`true`)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(scalarBody)); got != `{"value":true}` {
		t.Fatalf("scalar body = %s", got)
	}
	if err := client.PutAPIKeys(ctx, []management.SecretValue{"sk-one", "sk-two"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(keysBody)); got != `["sk-one","sk-two"]` {
		t.Fatalf("api-key body = %s", got)
	}
	if err := client.DeleteAPIKey(ctx, fingerprint("sk-second-secret")); err != nil {
		t.Fatal(err)
	}
	if deleteIndex != "1" {
		t.Fatalf("delete index = %q", deleteIndex)
	}
	usage, err := client.APIKeyUsage(ctx)
	if err != nil || len(usage) != 1 || usage[0].Requests != 5 || usage[0].Fingerprint != fingerprint("sk-usage-secret") {
		t.Fatalf("usage = %#v, %v", usage, err)
	}
	page, err := client.Logs(ctx, management.LogQuery{Since: time.Unix(1700000000, 0), Tail: 2})
	if err != nil || page.Next != "cursor-2" || len(page.Records) != 1 || strings.Contains(page.Records[0].Message, testProxyKey) {
		t.Fatalf("logs = %#v, %v", page, err)
	}
	if logsAfter != "1700000000" {
		t.Fatalf("logs after = %q", logsAfter)
	}
	if records, err := client.RequestErrorLogs(ctx); err != nil || len(records) != 1 {
		t.Fatalf("request errors = %#v, %v", records, err)
	}
	if version, err := client.LatestVersion(ctx); err != nil || version != "7.2.93" {
		t.Fatalf("latest = %q, %v", version, err)
	}
}

func TestPutConfigYAMLCompensatesOnVerificationMismatch(t *testing.T) {
	prior := []byte("port: 8317\n")
	candidate := []byte("port: 9000\n")
	state := append([]byte(nil), prior...)
	var sequence []string
	putCount := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sequence = append(sequence, r.Method)
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write(state)
		case http.MethodPut:
			putCount++
			body, _ := io.ReadAll(r.Body)
			if putCount == 1 {
				state = []byte("port: 9999\n")
			} else {
				state = body
			}
		}
	})
	err := client.PutConfigYAML(context.Background(), candidate)
	assertPMuxCode(t, err, pmuxerr.ConfigMutationConflict)
	if string(state) != string(prior) {
		t.Fatalf("state after compensation = %q", state)
	}
	want := "GET,PUT,GET,PUT,GET"
	if got := strings.Join(sequence, ","); got != want {
		t.Fatalf("sequence = %s, want %s", got, want)
	}
}

func TestPutConfigYAMLDoesNotRetryAfterVerificationAuthFailure(t *testing.T) {
	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		switch attempts {
		case 1:
			_, _ = w.Write([]byte("port: 8317\n"))
		case 2:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	err := client.PutConfigYAML(context.Background(), []byte("port: 9000\n"))
	assertPMuxCode(t, err, pmuxerr.ManagementAuthRejected)
	if attempts != 3 {
		t.Fatalf("authenticated attempts = %d, want 3 with no restore retry", attempts)
	}
}

func TestErrorsAndReturnedRecordsNeverExposeSecrets(t *testing.T) {
	secretBody := `{"message":"Bearer ` + testManagementKey + ` and ` + testProxyKey + `"}`
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(secretBody))
	})
	_, err := client.Config(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *pmuxerr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T", err)
	}
	serialized := string(mustJSON(t, pe))
	for _, secret := range []string{testManagementKey, testProxyKey} {
		if strings.Contains(err.Error(), secret) || strings.Contains(serialized, secret) {
			t.Fatalf("secret leaked: %q", secret)
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertPMuxCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	var pe *pmuxerr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error type = %T, want *pmuxerr.Error", err)
	}
	if pe.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", pe.Code, code, err)
	}
}

func isProviderPath(value string) bool {
	for _, kind := range []management.ProviderKeyKind{management.ProviderGemini, management.ProviderInteractions, management.ProviderClaude, management.ProviderCodex, management.ProviderXAI, management.ProviderVertex, management.ProviderOpenAICompatible} {
		if value == managementPrefix+"/"+string(kind) {
			return true
		}
	}
	return false
}
