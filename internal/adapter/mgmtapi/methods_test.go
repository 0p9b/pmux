package mgmtapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestPluginsListDecodesWireShape(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != managementPrefix+"/plugins" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testManagementKey {
			t.Errorf("management authorization = %q", got)
		}
		writeJSON(t, w, map[string]any{
			"plugins_enabled": true,
			"plugins_dir":     "/var/lib/cliproxyapi/plugins",
			"plugins": []map[string]any{{
				"id":                "oidc-login",
				"path":              "/var/lib/cliproxyapi/plugins/oidc-login.so",
				"configured":        true,
				"registered":        true,
				"enabled":           true,
				"effective_enabled": false,
				"supports_oauth":    true,
				"oauth_provider":    "oidc",
				"logo":              "https://example.com/logo.png",
				"config_fields": []map[string]any{{
					"name":        "mode",
					"type":        "enum",
					"enum_values": []string{"strict", "lax"},
					"description": "Validation mode",
				}},
				"menus": []map[string]any{{
					"path":        "/settings/oidc",
					"menu":        "OIDC",
					"description": "OIDC settings",
				}},
				"metadata": map[string]any{
					"name":              "OIDC Login",
					"version":           "1.2.3",
					"author":            "example",
					"github_repository": "example/oidc-login",
					"logo":              "https://example.com/meta-logo.png",
				},
			}},
		})
	})
	list, err := client.Plugins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !list.PluginsEnabled || list.PluginsDir != "/var/lib/cliproxyapi/plugins" {
		t.Fatalf("Plugins envelope: %#v", list)
	}
	if len(list.Plugins) != 1 {
		t.Fatalf("Plugins count = %d", len(list.Plugins))
	}
	plugin := list.Plugins[0]
	if plugin.ID != "oidc-login" || !plugin.Configured || !plugin.Registered || !plugin.Enabled || plugin.EffectiveEnabled {
		t.Fatalf("plugin flags: %#v", plugin)
	}
	if !plugin.SupportsOAuth || plugin.OAuthProvider != "oidc" || plugin.Logo != "https://example.com/logo.png" {
		t.Fatalf("plugin oauth/logo: %#v", plugin)
	}
	if len(plugin.ConfigFields) != 1 || plugin.ConfigFields[0].Name != "mode" || plugin.ConfigFields[0].Type != "enum" ||
		len(plugin.ConfigFields[0].EnumValues) != 2 || plugin.ConfigFields[0].EnumValues[1] != "lax" || plugin.ConfigFields[0].Description != "Validation mode" {
		t.Fatalf("config fields: %#v", plugin.ConfigFields)
	}
	if len(plugin.Menus) != 1 || plugin.Menus[0].Path != "/settings/oidc" || plugin.Menus[0].Menu != "OIDC" {
		t.Fatalf("menus: %#v", plugin.Menus)
	}
	if plugin.Metadata.Name != "OIDC Login" || plugin.Metadata.Version != "1.2.3" ||
		plugin.Metadata.Author != "example" || plugin.Metadata.GitHubRepository != "example/oidc-login" {
		t.Fatalf("metadata: %#v", plugin.Metadata)
	}
}

func TestPluginStoreDecodesWireShape(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != managementPrefix+"/plugin-store" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		writeJSON(t, w, map[string]any{
			"plugins_enabled": false,
			"plugins_dir":     "/plugins",
			"sources":         []map[string]any{{"id": "official", "name": "Official", "url": "https://store.example/index.json"}},
			"source_errors":   []map[string]any{{"source_id": "mirror", "source_name": "Mirror", "source_url": "https://mirror.example/index.json", "message": "unreachable"}},
			"plugins": []map[string]any{{
				"store_id":          "official:oidc-login",
				"source_id":         "official",
				"source_name":       "Official",
				"id":                "oidc-login",
				"name":              "OIDC Login",
				"description":       "OIDC login flow",
				"author":            "example",
				"version":           "1.2.3",
				"repository":        "https://github.com/example/oidc-login",
				"install_type":      "binary",
				"auth_required":     true,
				"auth_configured":   true,
				"platforms":         []map[string]any{{"goos": "linux", "goarch": "amd64"}},
				"homepage":          "https://example.com",
				"license":           "MIT",
				"tags":              []string{"auth", "oidc"},
				"installed":         true,
				"installed_version": "1.2.0",
				"update_available":  true,
			}},
		})
	})
	list, err := client.PluginStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if list.PluginsEnabled || list.PluginsDir != "/plugins" {
		t.Fatalf("PluginStore envelope: %#v", list)
	}
	if len(list.Sources) != 1 || list.Sources[0].ID != "official" || list.Sources[0].URL != "https://store.example/index.json" {
		t.Fatalf("sources: %#v", list.Sources)
	}
	if len(list.SourceErrors) != 1 || list.SourceErrors[0].SourceID != "mirror" || list.SourceErrors[0].Message != "unreachable" {
		t.Fatalf("source errors: %#v", list.SourceErrors)
	}
	if len(list.Plugins) != 1 {
		t.Fatalf("store plugin count = %d", len(list.Plugins))
	}
	plugin := list.Plugins[0]
	if plugin.StoreID != "official:oidc-login" || plugin.ID != "oidc-login" || plugin.InstallType != "binary" {
		t.Fatalf("store plugin identity: %#v", plugin)
	}
	if !plugin.AuthRequired || !plugin.AuthConfigured || !plugin.Installed || plugin.InstalledVersion != "1.2.0" || !plugin.UpdateAvailable {
		t.Fatalf("store plugin state: %#v", plugin)
	}
	if len(plugin.Platforms) != 1 || plugin.Platforms[0].GOOS != "linux" || plugin.Platforms[0].GOARCH != "amd64" {
		t.Fatalf("platforms: %#v", plugin.Platforms)
	}
	if len(plugin.Tags) != 2 || plugin.Tags[0] != "auth" || plugin.License != "MIT" || plugin.Homepage != "https://example.com" {
		t.Fatalf("tags/license/homepage: %#v", plugin)
	}
}

func TestInstallPluginSendsSourceAndVersion(t *testing.T) {
	var method, escapedPath, source, version string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		escapedPath = r.URL.EscapedPath()
		source = r.URL.Query().Get("source")
		version = r.URL.Query().Get("version")
		writeJSON(t, w, map[string]any{
			"status":           "installed",
			"id":               "oidc-login",
			"version":          "1.2.3",
			"path":             "/plugins/oidc-login.so",
			"plugins_enabled":  true,
			"restart_required": false,
		})
	})
	result, err := client.InstallPlugin(context.Background(), "oidc login/v2", "official", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || escapedPath != managementPrefix+"/plugin-store/oidc%20login%2Fv2/install" {
		t.Fatalf("install request = %s %s", method, escapedPath)
	}
	if source != "official" || version != "1.2.3" {
		t.Fatalf("install query source=%q version=%q", source, version)
	}
	if result.Status != "installed" || result.ID != "oidc-login" || result.Version != "1.2.3" ||
		result.Path != "/plugins/oidc-login.so" || !result.PluginsEnabled || result.RestartRequired {
		t.Fatalf("install result: %#v", result)
	}
}

func TestInstallPluginConflictSurfacesRestartDetail(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		writeJSON(t, w, map[string]any{"error": "plugin_update_requires_restart", "restart_required": true})
	})
	_, err := client.InstallPlugin(context.Background(), "oidc-login", "official", "")
	assertPMuxCode(t, err, pmuxerr.ConfigMutationConflict)
	if !strings.Contains(err.Error(), "plugin_update_requires_restart") || !strings.Contains(err.Error(), "restart_required=true") {
		t.Fatalf("restart detail absent: %v", err)
	}
	if strings.Contains(err.Error(), testManagementKey) {
		t.Fatalf("conflict error leaks the management key: %v", err)
	}
}

func TestSetPluginEnabledSendsBooleanBody(t *testing.T) {
	var method, escapedPath string
	var payload map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		escapedPath = r.URL.EscapedPath()
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := client.SetPluginEnabled(context.Background(), "oidc-login", false); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || escapedPath != managementPrefix+"/plugins/oidc-login/enabled" {
		t.Fatalf("enable request = %s %s", method, escapedPath)
	}
	if enabled, ok := payload["enabled"]; !ok || enabled != false || len(payload) != 1 {
		t.Fatalf("enable body = %v", payload)
	}
}

func TestPluginConfigDecodesArbitraryObject(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != managementPrefix+"/plugins/oidc-login/config" || r.Method != http.MethodGet {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
			}
			writeJSON(t, w, map[string]any{"issuer": "https://idp.example", "retries": 7, "nested": map[string]any{"flag": true}})
		})
		config, err := client.PluginConfig(context.Background(), "oidc-login")
		if err != nil {
			t.Fatal(err)
		}
		if config["issuer"] != "https://idp.example" || config["retries"] != json.Number("7") {
			t.Fatalf("config = %#v", config)
		}
		nested, ok := config["nested"].(map[string]any)
		if !ok || nested["flag"] != true {
			t.Fatalf("nested config = %#v", config["nested"])
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{})
		})
		config, err := client.PluginConfig(context.Background(), "oidc-login")
		if err != nil {
			t.Fatal(err)
		}
		if config == nil || len(config) != 0 {
			t.Fatalf("unconfigured config = %#v", config)
		}
	})
}

func TestPluginConfigNotFoundIsTyped(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == managementPrefix+"/config" {
			writeJSON(t, w, map[string]any{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"error": "plugin_not_found"})
	})
	_, err := client.PluginConfig(context.Background(), "missing-plugin")
	assertPMuxCode(t, err, pmuxerr.UnhandledUpstreamShape)
}

func TestPutPluginConfigSendsFullObject(t *testing.T) {
	var method string
	var body []byte
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	config := map[string]any{"issuer": "https://idp.example", "scopes": []string{"openid", "email"}}
	if err := client.PutPluginConfig(context.Background(), "oidc-login", config); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut {
		t.Fatalf("put method = %s", method)
	}
	var observed map[string]any
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatalf("put body is not JSON: %v", err)
	}
	if observed["issuer"] != "https://idp.example" {
		t.Fatalf("put body = %s", body)
	}
	scopes, ok := observed["scopes"].([]any)
	if !ok || len(scopes) != 2 || scopes[1] != "email" {
		t.Fatalf("put scopes = %s", body)
	}
}

func TestPatchPluginConfigPreservesNullDeletes(t *testing.T) {
	var method string
	var body []byte
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	patch := map[string]any{"issuer": "https://idp2.example", "obsolete": nil}
	if err := client.PatchPluginConfig(context.Background(), "oidc-login", patch); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch {
		t.Fatalf("patch method = %s", method)
	}
	var observed map[string]any
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatalf("patch body is not JSON: %v", err)
	}
	deleted, present := observed["obsolete"]
	if !present || deleted != nil {
		t.Fatalf("patch null delete not preserved: %s", body)
	}
	if observed["issuer"] != "https://idp2.example" {
		t.Fatalf("patch body = %s", body)
	}
}

func TestDeletePluginDecodesResult(t *testing.T) {
	var method, escapedPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		escapedPath = r.URL.EscapedPath()
		writeJSON(t, w, map[string]any{
			"status":             "deleted",
			"id":                 "oidc-login",
			"path":               "/plugins/oidc-login.so",
			"file_deleted":       true,
			"configured_removed": true,
			"restart_required":   false,
		})
	})
	result, err := client.DeletePlugin(context.Background(), "oidc-login")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || escapedPath != managementPrefix+"/plugins/oidc-login" {
		t.Fatalf("delete request = %s %s", method, escapedPath)
	}
	if result.ID != "oidc-login" || result.Path != "/plugins/oidc-login.so" ||
		!result.FileDeleted || !result.ConfigRemoved || result.RestartRequired {
		t.Fatalf("delete result: %#v", result)
	}
}

func TestDeletePluginConflictSurfacesRestartDetail(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		writeJSON(t, w, map[string]any{"error": "plugin_delete_requires_restart", "restart_required": true})
	})
	_, err := client.DeletePlugin(context.Background(), "oidc-login")
	assertPMuxCode(t, err, pmuxerr.ConfigMutationConflict)
	if !strings.Contains(err.Error(), "plugin_delete_requires_restart") || !strings.Contains(err.Error(), "restart_required=true") {
		t.Fatalf("restart detail absent: %v", err)
	}
}

func TestPluginMutationsKeepManagementErrorTaxonomy(t *testing.T) {
	t.Run("auth rejected", func(t *testing.T) {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := client.DeletePlugin(context.Background(), "oidc-login")
		assertPMuxCode(t, err, pmuxerr.ManagementAuthRejected)
		_, err = client.InstallPlugin(context.Background(), "oidc-login", "official", "")
		assertPMuxCode(t, err, pmuxerr.ManagementAuthRejected)
	})
	t.Run("management key missing", func(t *testing.T) {
		client, err := New(Options{BaseURL: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.DeletePlugin(context.Background(), "oidc-login")
		assertPMuxCode(t, err, pmuxerr.ConfigUnreadable)
	})
}
