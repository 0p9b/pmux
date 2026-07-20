//go:build compat

package mgmtapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"testing"

	"github.com/0p9b/pmux/internal/domain/management"
)

const (
	compatBaseURLEnv       = "PMUX_COMPAT_BASE_URL"
	compatManagementKeyEnv = "PMUX_COMPAT_MANAGEMENT_KEY"
	compatProxyKeyEnv      = "PMUX_COMPAT_PROXY_KEY"
)

// TestRealCoreContract is the release compatibility gate for the two supported
// CLIProxyAPI binaries. The workflow supplies a fresh temporary instance, so
// read-before/write-same/verify/restore transactions are non-destructive.
// Operations that could begin external authentication, import credentials, or
// issue arbitrary outbound requests are probed without invoking them.
func TestRealCoreContract(t *testing.T) {
	baseURL := requireCompatEnv(t, compatBaseURLEnv)
	managementKey := requireCompatEnv(t, compatManagementKeyEnv)
	proxyKey := requireCompatEnv(t, compatProxyKeyEnv)
	client, err := New(Options{BaseURL: baseURL, ManagementKey: managementKey, ProxyKey: proxyKey})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("machine-readable surfaces", func(t *testing.T) {
		info, err := client.Health(ctx)
		if err != nil || !info.Healthy {
			t.Fatalf("health: %#v, %v", info, err)
		}
		if _, err := client.PublicModels(ctx); err != nil {
			t.Fatalf("public models: %v", err)
		}
		capabilities, err := client.Capabilities(ctx)
		if err != nil {
			t.Fatalf("capabilities: %v", err)
		}
		for _, required := range []string{
			"management", "management-config-yaml", "management-api-keys",
			"management-provider-keys", "management-oauth", "management-auth-files",
			"management-model-attribution", "management-logs", "management-vertex-import",
			"management-reset-quota", "management-latest-version", "management-api-call",
		} {
			if !capabilities[required] {
				t.Errorf("required capability %q was not probed successfully", required)
			}
		}
	})

	t.Run("config read put verify restore", func(t *testing.T) {
		if _, err := client.Config(ctx); err != nil {
			t.Fatalf("config object: %v", err)
		}
		before, err := client.ConfigYAML(ctx)
		if err != nil {
			t.Fatalf("config yaml: %v", err)
		}
		if err := client.PutConfigYAML(ctx, before); err != nil {
			t.Fatalf("put same config: %v", err)
		}
		after, err := client.ConfigYAML(ctx)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("config changed after put-same: equal=%v, err=%v", bytes.Equal(before, after), err)
		}
	})

	t.Run("scalar resources reversible put", func(t *testing.T) {
		names := make([]management.SettingName, 0, len(settings))
		for name := range settings {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
		for _, name := range names {
			name := name
			t.Run(string(name), func(t *testing.T) {
				before, err := client.GetSetting(ctx, name)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if err := client.PutSetting(ctx, name, before); err != nil {
					t.Fatalf("put same: %v", err)
				}
				after, err := client.GetSetting(ctx, name)
				if err != nil || !sameJSON(before, after) {
					t.Fatalf("setting changed after put-same: equal=%v, err=%v", sameJSON(before, after), err)
				}
			})
		}
	})

	t.Run("client keys reversible put", func(t *testing.T) {
		before, err := client.APIKeys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != 1 || before[0].Fingerprint != fingerprint(proxyKey) {
			t.Fatalf("temporary core key inventory did not match the supplied proxy key: %#v", before)
		}
		if err := client.PutAPIKeys(ctx, []management.SecretValue{management.SecretValue(proxyKey)}); err != nil {
			t.Fatalf("put same client key: %v", err)
		}
		after, err := client.APIKeys(ctx)
		if err != nil || len(after) != 1 || after[0].Fingerprint != fingerprint(proxyKey) {
			t.Fatalf("client key verification failed: %#v, %v", after, err)
		}
		if _, err := client.APIKeyUsage(ctx); err != nil {
			t.Fatalf("api key usage: %v", err)
		}
	})

	t.Run("provider key resources reversible empty put", func(t *testing.T) {
		for _, kind := range []management.ProviderKeyKind{
			management.ProviderGemini, management.ProviderInteractions, management.ProviderClaude,
			management.ProviderCodex, management.ProviderXAI, management.ProviderVertex,
			management.ProviderOpenAICompatible,
		} {
			kind := kind
			t.Run(string(kind), func(t *testing.T) {
				before, err := client.ProviderKeys(ctx, kind)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if len(before) != 0 {
					t.Fatalf("compat instance must start without provider secrets; got %d redacted entries", len(before))
				}
				if err := client.PutProviderKeys(ctx, kind, before); err != nil {
					t.Fatalf("put same empty collection: %v", err)
				}
				after, err := client.ProviderKeys(ctx, kind)
				if err != nil || len(after) != 0 {
					t.Fatalf("provider collection changed: %#v, %v", after, err)
				}
			})
		}
	})

	t.Run("oauth model controls reversible put", func(t *testing.T) {
		excluded, err := client.ExcludedModels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.PutExcludedModels(ctx, excluded); err != nil {
			t.Fatalf("put excluded models: %v", err)
		}
		excludedAfter, err := client.ExcludedModels(ctx)
		if err != nil || !sameJSONValue(excluded, excludedAfter) {
			t.Fatalf("excluded models changed: %#v, %v", excludedAfter, err)
		}
		aliases, err := client.ModelAliases(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.PutModelAliases(ctx, aliases); err != nil {
			t.Fatalf("put aliases: %v", err)
		}
		aliasesAfter, err := client.ModelAliases(ctx)
		if err != nil || !sameJSONValue(aliases, aliasesAfter) {
			t.Fatalf("aliases changed: %#v, %v", aliasesAfter, err)
		}
	})

	t.Run("auth inventory and model attribution reads", func(t *testing.T) {
		files, err := client.AuthFiles(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if _, err := client.AuthFileModels(ctx, file.Name); err != nil {
				t.Errorf("models for %q: %v", file.Name, err)
			}
		}
		for _, channel := range []string{"claude", "codex", "antigravity", "kimi", "xai", "gemini", "vertex"} {
			if _, err := client.ModelDefinitions(ctx, channel); err != nil {
				t.Errorf("model definitions for %q: %v", channel, err)
			}
		}
	})

	t.Run("logs and administrative reads", func(t *testing.T) {
		logging, err := client.GetSetting(ctx, management.SettingName("logging-to-file"))
		if err != nil {
			t.Fatalf("read logging setting: %v", err)
		}
		if err := client.PutSetting(ctx, management.SettingName("logging-to-file"), management.SettingValue(`true`)); err != nil {
			t.Fatalf("enable logging for contract read: %v", err)
		}
		defer func() {
			if err := client.PutSetting(context.Background(), management.SettingName("logging-to-file"), logging); err != nil {
				t.Errorf("restore logging setting: %v", err)
			}
		}()
		if _, err := client.Logs(ctx, management.LogQuery{Tail: 10}); err != nil {
			t.Errorf("logs: %v", err)
		}
		errorLogs, err := client.RequestErrorLogs(ctx)
		if err != nil {
			t.Errorf("request error logs: %v", err)
		}
		for _, record := range errorLogs {
			if _, err := client.RequestErrorLog(ctx, record.Name); err != nil {
				t.Errorf("request error log %q: %v", record.Name, err)
			}
		}
		if _, err := client.PopUsageQueue(ctx); err != nil {
			t.Errorf("empty usage queue: %v", err)
		}
		if _, err := client.LatestVersion(ctx); err != nil {
			t.Errorf("latest version: %v", err)
		}
	})

	t.Run("unsafe operations validation probe only", func(t *testing.T) {
		for _, endpoint := range []string{
			"anthropic-auth-url", "codex-auth-url", "antigravity-auth-url", "kimi-auth-url", "xai-auth-url",
			"get-auth-status", "oauth-callback", "oauth-session", "vertex/import", "reset-quota", "api-call",
			"auth-files/status", "auth-files/fields", "request-error-logs/probe", "request-log-by-id/probe",
		} {
			if ok, err := client.probe(ctx, probeSpec{method: http.MethodOptions, endpoint: endpoint}); err != nil || !ok {
				t.Errorf("OPTIONS probe %s: available=%v, err=%v", endpoint, ok, err)
			}
		}
	})
}

func requireCompatEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for the compat-tagged real-core contract", name)
	}
	return value
}

func sameJSON(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		sameJSONValue(leftValue, rightValue)
}

func sameJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
