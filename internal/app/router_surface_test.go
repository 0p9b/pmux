package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

func surfaceInstallation() state.Installation {
	return state.Installation{ID: "default", Kind: "managed", ProxyKeyRef: state.SecretReference{Path: "/tmp/key"}, Host: "127.0.0.1", Port: 8317}
}

type surfaceManagement struct {
	management.ManagementClient
	keys         []management.SecretRef
	usage        []management.APIKeyUsage
	keyPatches   []management.KeyPatch
	deletedKeys  []string
	aliases      management.ModelAliasSet
	putAliases   []management.ModelAliasSet
	clearAliases []string
	excluded     management.ExcludedModelSet
	putExcluded  []management.ExcludedModelSet
	clearExcl    []string
	quotaResets  []management.ResetQuotaRequest
	pluginList   management.PluginList
	pluginStore  management.PluginStoreList
	installed    []string
	enabled      map[string]bool
	pluginCfgs   map[string]map[string]any
	putCfgs      map[string]map[string]any
	patchCfgs    map[string]map[string]any
	deletedPlg   []string
	installRes   management.PluginInstallResult
	deleteRes    management.PluginDeleteResult
}

func (m *surfaceManagement) APIKeys(context.Context) ([]management.SecretRef, error) {
	return m.keys, nil
}
func (m *surfaceManagement) APIKeyUsage(context.Context) ([]management.APIKeyUsage, error) {
	return m.usage, nil
}
func (m *surfaceManagement) PatchAPIKeys(_ context.Context, patch management.KeyPatch) error {
	m.keyPatches = append(m.keyPatches, patch)
	return nil
}
func (m *surfaceManagement) DeleteAPIKey(_ context.Context, fingerprint string) error {
	m.deletedKeys = append(m.deletedKeys, fingerprint)
	return nil
}
func (m *surfaceManagement) ModelAliases(context.Context) (management.ModelAliasSet, error) {
	return m.aliases, nil
}
func (m *surfaceManagement) PutModelAliases(_ context.Context, value management.ModelAliasSet) error {
	m.putAliases = append(m.putAliases, value)
	m.aliases = value
	return nil
}
func (m *surfaceManagement) DeleteModelAliases(_ context.Context, channel string) error {
	m.clearAliases = append(m.clearAliases, channel)
	return nil
}
func (m *surfaceManagement) ExcludedModels(context.Context) (management.ExcludedModelSet, error) {
	return m.excluded, nil
}
func (m *surfaceManagement) PutExcludedModels(_ context.Context, value management.ExcludedModelSet) error {
	m.putExcluded = append(m.putExcluded, value)
	m.excluded = value
	return nil
}
func (m *surfaceManagement) DeleteExcludedModels(_ context.Context, channel string) error {
	m.clearExcl = append(m.clearExcl, channel)
	return nil
}
func (m *surfaceManagement) ResetQuota(_ context.Context, request management.ResetQuotaRequest) error {
	m.quotaResets = append(m.quotaResets, request)
	return nil
}
func (m *surfaceManagement) Plugins(context.Context) (management.PluginList, error) {
	return m.pluginList, nil
}
func (m *surfaceManagement) PluginStore(context.Context) (management.PluginStoreList, error) {
	return m.pluginStore, nil
}
func (m *surfaceManagement) InstallPlugin(_ context.Context, id, _, _ string) (management.PluginInstallResult, error) {
	m.installed = append(m.installed, id)
	return m.installRes, nil
}
func (m *surfaceManagement) SetPluginEnabled(_ context.Context, id string, enabled bool) error {
	if m.enabled == nil {
		m.enabled = map[string]bool{}
	}
	m.enabled[id] = enabled
	return nil
}
func (m *surfaceManagement) PluginConfig(_ context.Context, id string) (map[string]any, error) {
	if m.pluginCfgs == nil {
		return map[string]any{}, nil
	}
	return m.pluginCfgs[id], nil
}
func (m *surfaceManagement) PutPluginConfig(_ context.Context, id string, config map[string]any) error {
	if m.putCfgs == nil {
		m.putCfgs = map[string]map[string]any{}
	}
	m.putCfgs[id] = config
	return nil
}
func (m *surfaceManagement) PatchPluginConfig(_ context.Context, id string, patch map[string]any) error {
	if m.patchCfgs == nil {
		m.patchCfgs = map[string]map[string]any{}
	}
	m.patchCfgs[id] = patch
	return nil
}
func (m *surfaceManagement) DeletePlugin(_ context.Context, id string) (management.PluginDeleteResult, error) {
	m.deletedPlg = append(m.deletedPlg, id)
	return m.deleteRes, nil
}

func surfaceRouter(t *testing.T, mgmt management.ManagementClient, store *memoryStore) *Router {
	t.Helper()
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store,
		Management: func(context.Context, state.Installation) (management.ManagementClient, error) { return mgmt, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func noninteractive(options map[string]any) Invocation {
	return Invocation{Interactive: false, Yes: true, JSON: true, Options: options}
}

func TestKeysListIncludesUsage(t *testing.T) {
	mgmt := &surfaceManagement{
		keys:  []management.SecretRef{{Mask: "sk-...1", Fingerprint: "fp1"}},
		usage: []management.APIKeyUsage{{Fingerprint: "fp1", Requests: 42}},
	}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	result, err := router.Execute(context.Background(), Invocation{Operation: OpKeysList, JSON: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := result.Data.(map[string]any)["keys"].([]map[string]any)
	if len(keys) != 1 || keys[0]["requests"].(int64) != 42 {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestKeysAddGenerateReturnsKeyOnce(t *testing.T) {
	mgmt := &surfaceManagement{}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	inv := noninteractive(map[string]any{"generate": true})
	inv.Operation = OpKeysAdd
	result, err := router.Execute(context.Background(), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := result.Data.(map[string]any)["api_key"].(string)
	if len(key) != 64 {
		t.Fatalf("generated key length = %d", len(key))
	}
	if len(mgmt.keyPatches) != 1 || !strings.Contains(string(mgmt.keyPatches[0]), key) {
		t.Fatalf("patches = %#v", mgmt.keyPatches)
	}
}

func TestKeysAddRejectsEmptyStdin(t *testing.T) {
	mgmt := &surfaceManagement{}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	router.deps.Input = strings.NewReader("\n")
	inv := noninteractive(map[string]any{"api_key_stdin": true})
	inv.Operation = OpKeysAdd
	_, err := router.Execute(context.Background(), inv, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v", err)
	}
	if len(mgmt.keyPatches) != 0 {
		t.Fatal("empty key must not be patched")
	}
}

func TestKeysRemoveByFingerprint(t *testing.T) {
	mgmt := &surfaceManagement{}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	inv := noninteractive(nil)
	inv.Operation = OpKeysRemove
	inv.Arguments = []string{"fp1"}
	if _, err := router.Execute(context.Background(), inv, nil); err != nil {
		t.Fatal(err)
	}
	if len(mgmt.deletedKeys) != 1 || mgmt.deletedKeys[0] != "fp1" {
		t.Fatalf("deleted = %#v", mgmt.deletedKeys)
	}
}

func TestKeysAddNoninteractiveRequiresYes(t *testing.T) {
	router := surfaceRouter(t, &surfaceManagement{}, configuredState(surfaceInstallation()))
	inv := Invocation{Operation: OpKeysAdd, Interactive: false, Yes: false, JSON: true, Options: map[string]any{"generate": true}}
	_, err := router.Execute(context.Background(), inv, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v", err)
	}
}

func TestModelsAliasesSetAndRemove(t *testing.T) {
	mgmt := &surfaceManagement{}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	set := noninteractive(map[string]any{"action": "set"})
	set.Operation = OpModelsAliases
	set.Arguments = []string{"codex", "fast", "exact/model-1"}
	if _, err := router.Execute(context.Background(), set, nil); err != nil {
		t.Fatal(err)
	}
	if mgmt.aliases["codex"]["fast"] != "exact/model-1" {
		t.Fatalf("aliases = %#v", mgmt.aliases)
	}
	remove := noninteractive(map[string]any{"action": "remove"})
	remove.Operation = OpModelsAliases
	remove.Arguments = []string{"codex", "fast"}
	if _, err := router.Execute(context.Background(), remove, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgmt.aliases["codex"]; ok {
		t.Fatalf("empty channel must be dropped: %#v", mgmt.aliases)
	}
}

func TestModelsAliasesRejectsUnknownChannel(t *testing.T) {
	router := surfaceRouter(t, &surfaceManagement{}, configuredState(surfaceInstallation()))
	inv := noninteractive(map[string]any{"action": "set"})
	inv.Operation = OpModelsAliases
	inv.Arguments = []string{"bogus", "a", "m"}
	_, err := router.Execute(context.Background(), inv, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v", err)
	}
}

func TestModelsExclusionsAddRemoveClear(t *testing.T) {
	mgmt := &surfaceManagement{}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	add := noninteractive(map[string]any{"action": "add"})
	add.Operation = OpModelsExclusions
	add.Arguments = []string{"claude", "bad-*"}
	if _, err := router.Execute(context.Background(), add, nil); err != nil {
		t.Fatal(err)
	}
	// Duplicate add is idempotent.
	if _, err := router.Execute(context.Background(), add, nil); err != nil {
		t.Fatal(err)
	}
	if len(mgmt.excluded["claude"]) != 1 {
		t.Fatalf("excluded = %#v", mgmt.excluded)
	}
	remove := noninteractive(map[string]any{"action": "remove"})
	remove.Operation = OpModelsExclusions
	remove.Arguments = []string{"claude", "bad-*"}
	if _, err := router.Execute(context.Background(), remove, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgmt.excluded["claude"]; ok {
		t.Fatalf("empty channel must be dropped: %#v", mgmt.excluded)
	}
	// Removing a missing pattern is a usage error.
	_, err := router.Execute(context.Background(), remove, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v", err)
	}
	// Clear goes through the delete endpoint.
	add2 := noninteractive(map[string]any{"action": "add"})
	add2.Operation = OpModelsExclusions
	add2.Arguments = []string{"kimi", "x"}
	if _, err := router.Execute(context.Background(), add2, nil); err != nil {
		t.Fatal(err)
	}
	clear := noninteractive(map[string]any{"action": "clear"})
	clear.Operation = OpModelsExclusions
	clear.Arguments = []string{"kimi"}
	if _, err := router.Execute(context.Background(), clear, nil); err != nil {
		t.Fatal(err)
	}
	if len(mgmt.clearExcl) != 1 || mgmt.clearExcl[0] != "kimi" {
		t.Fatalf("cleared = %#v", mgmt.clearExcl)
	}
}

func TestProviderResetQuota(t *testing.T) {
	mgmt := &surfaceManagement{}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	inv := noninteractive(nil)
	inv.Operation = OpProvidersResetQuota
	inv.Arguments = []string{"claude-user.json"}
	if _, err := router.Execute(context.Background(), inv, nil); err != nil {
		t.Fatal(err)
	}
	if len(mgmt.quotaResets) != 1 || mgmt.quotaResets[0].Name != "claude-user.json" {
		t.Fatalf("resets = %#v", mgmt.quotaResets)
	}
}

func TestPluginsLifecycle(t *testing.T) {
	mgmt := &surfaceManagement{
		pluginList: management.PluginList{
			PluginsEnabled: true, PluginsDir: "/plugins",
			Plugins: []management.PluginInfo{{ID: "sample", Enabled: true, Registered: true, EffectiveEnabled: true, Metadata: management.PluginMetadata{Version: "1.0.0"}}},
		},
		installRes: management.PluginInstallResult{Status: "installed", ID: "sample", Version: "1.0.0"},
		deleteRes:  management.PluginDeleteResult{ID: "sample", FileDeleted: true},
	}
	router := surfaceRouter(t, mgmt, configuredState(surfaceInstallation()))
	ctx := context.Background()

	result, err := router.Execute(ctx, Invocation{Operation: OpPluginsList, JSON: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Data.(management.PluginList).Plugins[0].ID != "sample" {
		t.Fatalf("data = %#v", result.Data)
	}

	enable := noninteractive(map[string]any{"enabled": true})
	enable.Operation = OpPluginSetEnabled
	enable.Arguments = []string{"sample"}
	if _, err := router.Execute(ctx, enable, nil); err != nil {
		t.Fatal(err)
	}
	if !mgmt.enabled["sample"] {
		t.Fatal("enable not recorded")
	}

	show := noninteractive(nil)
	show.Operation = OpPluginConfigShow
	show.Arguments = []string{"sample"}
	if _, err := router.Execute(ctx, show, nil); err != nil {
		t.Fatal(err)
	}

	set := noninteractive(map[string]any{"patch": false})
	set.Operation = OpPluginConfigSet
	set.Arguments = []string{"sample", `{"priority":1}`}
	if _, err := router.Execute(ctx, set, nil); err != nil {
		t.Fatal(err)
	}
	if mgmt.putCfgs["sample"]["priority"].(float64) != 1 {
		t.Fatalf("put = %#v", mgmt.putCfgs)
	}

	patch := noninteractive(map[string]any{"patch": true})
	patch.Operation = OpPluginConfigSet
	patch.Arguments = []string{"sample", `{"priority":null}`}
	if _, err := router.Execute(ctx, patch, nil); err != nil {
		t.Fatal(err)
	}
	if value, ok := mgmt.patchCfgs["sample"]["priority"]; !ok || value != nil {
		t.Fatalf("patch = %#v", mgmt.patchCfgs)
	}

	bad := noninteractive(nil)
	bad.Operation = OpPluginConfigSet
	bad.Arguments = []string{"sample", `[1,2]`}
	if _, err := router.Execute(ctx, bad, nil); err == nil {
		t.Fatal("non-object JSON must be rejected")
	}

	install := noninteractive(map[string]any{"source": "", "version": ""})
	install.Operation = OpPluginInstall
	install.Arguments = []string{"sample"}
	if _, err := router.Execute(ctx, install, nil); err != nil {
		t.Fatal(err)
	}
	if len(mgmt.installed) != 1 {
		t.Fatalf("installed = %#v", mgmt.installed)
	}

	remove := noninteractive(nil)
	remove.Operation = OpPluginRemove
	remove.Arguments = []string{"sample"}
	if _, err := router.Execute(ctx, remove, nil); err != nil {
		t.Fatal(err)
	}
	if len(mgmt.deletedPlg) != 1 {
		t.Fatalf("deleted = %#v", mgmt.deletedPlg)
	}
}

func TestPanelURLAndOpen(t *testing.T) {
	router := surfaceRouter(t, &surfaceManagement{}, configuredState(surfaceInstallation()))
	opened := ""
	router.deps.OpenURL = func(_ context.Context, url string) error { opened = url; return nil }
	inv := noninteractive(map[string]any{"open": true})
	inv.Operation = OpPanel
	result, err := router.Execute(context.Background(), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8317/management.html"
	data := result.Data.(map[string]any)
	if data["url"] != want || opened != want || data["opened"] != true {
		t.Fatalf("data = %#v opened = %q", result.Data, opened)
	}
}

func TestProfilesRoundTrip(t *testing.T) {
	store := configuredState(surfaceInstallation())
	router := surfaceRouter(t, &surfaceManagement{}, store)
	ctx := context.Background()

	set := noninteractive(map[string]any{"client": "codex", "model": "m1", "fallback": []string{"m2", "m3"}, "args": []string{"--quiet"}})
	set.Operation = OpProfilesSet
	set.Arguments = []string{"work"}
	if _, err := router.Execute(ctx, set, nil); err != nil {
		t.Fatal(err)
	}
	profile := store.config.Profiles["work"]
	if profile.Client != "codex" || profile.Model != "m1" || len(profile.Fallback) != 2 || profile.Args[0] != "--quiet" {
		t.Fatalf("profile = %#v", profile)
	}

	list, err := router.Execute(ctx, Invocation{Operation: OpProfilesList, JSON: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Data.(map[string]any)["profiles"].([]map[string]any)) != 1 {
		t.Fatalf("list = %#v", list.Data)
	}

	show, err := router.Execute(ctx, Invocation{Operation: OpProfilesShow, JSON: true, Arguments: []string{"work"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if show.Data.(map[string]any)["name"] != "work" {
		t.Fatalf("show = %#v", show.Data)
	}

	remove := noninteractive(nil)
	remove.Operation = OpProfilesRemove
	remove.Arguments = []string{"work"}
	if _, err := router.Execute(ctx, remove, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.config.Profiles["work"]; ok {
		t.Fatal("profile not removed")
	}
}

func TestProfilesSetValidation(t *testing.T) {
	store := configuredState(surfaceInstallation())
	router := surfaceRouter(t, &surfaceManagement{}, store)
	ctx := context.Background()

	badClient := noninteractive(map[string]any{"client": "bogus", "model": "m1"})
	badClient.Operation = OpProfilesSet
	badClient.Arguments = []string{"x"}
	if _, err := router.Execute(ctx, badClient, nil); err == nil {
		t.Fatal("invalid client must be rejected")
	}

	modelFlag := noninteractive(map[string]any{"client": "claude", "model": "m1", "args": []string{"--model", "other"}})
	modelFlag.Operation = OpProfilesSet
	modelFlag.Arguments = []string{"x"}
	if _, err := router.Execute(ctx, modelFlag, nil); err == nil {
		t.Fatal("model flag in profile args must be rejected")
	}

	emptyModel := noninteractive(map[string]any{"client": "claude", "model": ""})
	emptyModel.Operation = OpProfilesSet
	emptyModel.Arguments = []string{"x"}
	if _, err := router.Execute(ctx, emptyModel, nil); err == nil {
		t.Fatal("empty model must be rejected")
	}
	if len(store.config.Profiles) != 0 {
		t.Fatalf("no profile may persist on validation failure: %#v", store.config.Profiles)
	}
}

func TestLaunchSelectsFallbackWhenPrimaryUnavailable(t *testing.T) {
	store := configuredState(surfaceInstallation())
	catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{ID: "fallback-model", Available: true}}}
	launcher := &fakeLauncher{}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store,
		Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
		Launcher: func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error) {
			return launcher, nil
		},
		Secrets:    func(context.Context, state.Installation) ([]byte, error) { return []byte("token"), nil },
		WorkingDir: func() (string, error) { return "/tmp", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	inv := Invocation{
		Operation: OpLaunch, Interactive: false, Yes: true, JSON: true,
		Options: map[string]any{"client": "codex", "model": "missing-model", "fallback": []string{"fallback-model"}},
	}
	result, err := router.Execute(context.Background(), inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.launched.Model != "fallback-model" {
		t.Fatalf("launched model = %q", launcher.launched.Model)
	}
	if launcher.launched.Client != domainclient.Codex {
		t.Fatalf("launched client = %q", launcher.launched.Client)
	}
	data := result.Data.(map[string]any)
	if data["model"] != "fallback-model" || data["client"] != "codex" {
		t.Fatalf("data = %#v", result.Data)
	}
}

func TestLaunchFailsWhenNoCandidateServed(t *testing.T) {
	store := configuredState(surfaceInstallation())
	catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{ID: "other", Available: true}}}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store,
		Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
		Launcher: func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error) {
			return &fakeLauncher{}, nil
		},
		Secrets:    func(context.Context, state.Installation) ([]byte, error) { return []byte("token"), nil },
		WorkingDir: func() (string, error) { return "/tmp", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	inv := Invocation{
		Operation: OpLaunch, Interactive: false, Yes: true, JSON: true,
		Options: map[string]any{"client": "claude", "model": "missing", "fallback": []string{"also-missing"}},
	}
	_, err = router.Execute(context.Background(), inv, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUnhealthy {
		t.Fatalf("error = %#v", err)
	}
}

func TestLaunchUsesProfileDefaults(t *testing.T) {
	store := configuredState(surfaceInstallation())
	store.config.Profiles = map[string]state.Profile{
		"work": {Client: "gemini", Model: "missing", Fallback: []string{"live"}, Args: []string{"--quiet"}},
	}
	catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{ID: "live", Available: true}}}
	launcher := &fakeLauncher{}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store,
		Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
		Launcher: func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error) {
			return launcher, nil
		},
		Secrets:    func(context.Context, state.Installation) ([]byte, error) { return []byte("token"), nil },
		WorkingDir: func() (string, error) { return "/tmp", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	inv := Invocation{
		Operation: OpLaunch, Interactive: false, Yes: true, JSON: true,
		Options: map[string]any{"profile": "work"},
	}
	if _, err := router.Execute(context.Background(), inv, nil); err != nil {
		t.Fatal(err)
	}
	if launcher.launched.Client != domainclient.Gemini || launcher.launched.Model != "live" {
		t.Fatalf("launched = %#v", launcher.launched)
	}
	if len(launcher.launched.Args) != 1 || launcher.launched.Args[0] != "--quiet" {
		t.Fatalf("args = %#v", launcher.launched.Args)
	}
}

func TestLaunchRejectsUnknownProfile(t *testing.T) {
	store := configuredState(surfaceInstallation())
	router := surfaceRouter(t, &surfaceManagement{}, store)
	inv := noninteractive(map[string]any{"profile": "ghost"})
	inv.Operation = OpLaunch
	_, err := router.Execute(context.Background(), inv, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v", err)
	}
}

func TestLaunchRejectsUnsupportedClient(t *testing.T) {
	store := configuredState(surfaceInstallation())
	router := surfaceRouter(t, &surfaceManagement{}, store)
	inv := noninteractive(map[string]any{"client": "cursor", "model": "m"})
	inv.Operation = OpLaunch
	_, err := router.Execute(context.Background(), inv, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v", err)
	}
}

func TestParseExtendedProxyValue(t *testing.T) {
	cases := []struct {
		key, raw string
		want     any
		ok       bool
	}{
		{"routing.session-affinity", "true", true, true},
		{"routing.session-affinity-ttl", "1h", "1h", true},
		{"routing.session-affinity-ttl", "nonsense", nil, false},
		{"quota-exceeded.antigravity-credits", "false", false, true},
		{"max-retry-credentials", "3", 3, true},
		{"max-retry-credentials", "-1", nil, false},
		{"transient-error-cooldown-seconds", "-1", -1, true},
		{"transient-error-cooldown-seconds", "-2", nil, false},
		{"payload.override", `[{"models":["a"],"params":{"x":1}}]`, []any{map[string]any{"models": []any{"a"}, "params": map[string]any{"x": float64(1)}}}, true},
		{"payload.override", `{"not":"array"}`, nil, false},
		{"disable-image-generation", "chat", "chat", true},
		{"disable-image-generation", "true", true, true},
		{"disable-image-generation", "bogus", nil, false},
		{"commercial-mode", "yes", nil, false},
		{"unknown.extended", "1", nil, false},
	}
	for _, test := range cases {
		got, err := parseExtendedProxyValue(test.key, test.raw)
		if test.ok && err != nil {
			t.Fatalf("%s=%s: %v", test.key, test.raw, err)
		}
		if !test.ok && err == nil {
			t.Fatalf("%s=%s: expected rejection", test.key, test.raw)
		}
		if test.ok {
			wantJSON, _ := json.Marshal(test.want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Fatalf("%s=%s: got %#v want %#v", test.key, test.raw, got, test.want)
			}
		}
	}
}
