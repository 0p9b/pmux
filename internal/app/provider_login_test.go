package app

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

type providerFlowFake struct {
	flows        []provider.AuthFlow
	beginFlow    provider.AuthFlow
	pollStatuses []management.OAuthStatus
	pollErr      error
	polls        int
	paste        string
	application  provider.APIKeyApplication
	apiKey       string
	vertex       provider.VertexImport
	canceled     int
}

func (fake *providerFlowFake) Provider() management.ProviderID { return "fixture" }
func (fake *providerFlowFake) Flows() []provider.AuthFlow {
	return append([]provider.AuthFlow(nil), fake.flows...)
}
func (fake *providerFlowFake) Begin(_ context.Context, flow provider.AuthFlow) (provider.AuthSession, error) {
	fake.beginFlow = flow
	return provider.AuthSession{
		Provider: "fixture",
		Flow:     flow,
		Challenge: management.OAuthChallenge{
			State:           "oauth-state-must-not-render",
			URL:             "https://provider.invalid/authorize",
			VerificationURI: "https://provider.invalid/device",
			UserCode:        "ABCD-EFGH",
		},
	}, nil
}
func (fake *providerFlowFake) Poll(context.Context, provider.AuthSession) (management.OAuthStatus, error) {
	fake.polls++
	if fake.pollErr != nil {
		return management.OAuthStatus{}, fake.pollErr
	}
	if len(fake.pollStatuses) == 0 {
		return management.OAuthStatus{Status: "complete"}, nil
	}
	index := fake.polls - 1
	if index >= len(fake.pollStatuses) {
		index = len(fake.pollStatuses) - 1
	}
	return fake.pollStatuses[index], nil
}
func (fake *providerFlowFake) CompletePaste(_ context.Context, _ provider.AuthSession, callbackURL string) (management.OAuthStatus, error) {
	fake.paste = callbackURL
	return management.OAuthStatus{Status: "complete"}, nil
}
func (fake *providerFlowFake) ApplyAPIKey(ctx context.Context, application provider.APIKeyApplication) (management.ProviderKey, error) {
	fake.application = application
	value, err := application.Input.ReadSecret(ctx)
	if err != nil {
		return management.ProviderKey{}, err
	}
	fake.apiKey = strings.TrimSpace(string(value))
	for index := range value {
		value[index] = 0
	}
	// A defensive router must not serialize Fields even if an adapter regresses.
	return management.ProviderKey{ID: "configured", Mask: "********", Fields: map[string]string{"api-key": fake.apiKey}}, nil
}
func (fake *providerFlowFake) ImportVertex(_ context.Context, request provider.VertexImport) (management.VertexImportResult, error) {
	fake.vertex = request
	return management.VertexImportResult{Name: "vertex-project.json"}, nil
}
func (fake *providerFlowFake) Cancel(context.Context, provider.AuthSession) error {
	fake.canceled++
	return nil
}

type providerVerifyManagement struct {
	management.ManagementClient
	files   []management.AuthFile
	patches int
	deletes []string
}

func (fake *providerVerifyManagement) AuthFiles(context.Context) ([]management.AuthFile, error) {
	return append([]management.AuthFile(nil), fake.files...), nil
}

func (fake *providerVerifyManagement) PatchAuthFileStatus(context.Context, management.AuthFileStatusPatch) error {
	fake.patches++
	return nil
}

func (fake *providerVerifyManagement) DeleteAuthFiles(_ context.Context, names []string, _ bool) error {
	fake.deletes = append(fake.deletes, names...)
	return nil
}

type providerVerifyCatalog struct {
	entries   []domainmodel.CatalogEntry
	refreshes int
}

func (fake *providerVerifyCatalog) List(context.Context) ([]domainmodel.CatalogEntry, error) {
	return append([]domainmodel.CatalogEntry(nil), fake.entries...), nil
}
func (fake *providerVerifyCatalog) Refresh(context.Context) ([]domainmodel.CatalogEntry, error) {
	fake.refreshes++
	return append([]domainmodel.CatalogEntry(nil), fake.entries...), nil
}
func (fake *providerVerifyCatalog) Attribution(context.Context) (map[string][]management.ProviderID, error) {
	return map[string][]management.ProviderID{}, nil
}

func providerRouter(t *testing.T, fake *providerFlowFake, input string) *Router {
	t.Helper()
	installation := state.Installation{ID: "default", Kind: "managed"}
	router, err := NewRouter(Dependencies{
		Roots: testRoots(),
		Store: &memoryStore{state: state.State{Version: state.SchemaVersion, Installations: []state.Installation{installation}}},
		Input: strings.NewReader(input),
		Auth: func(context.Context, state.Installation, management.ProviderID) (provider.ProviderAuthenticator, error) {
			return fake, nil
		},
		VerifyPrivateFile: func(path string) error {
			if strings.Contains(path, "public-provider.key") {
				return errors.New("protected input permissions are not private")
			}
			return nil
		},
		ReadPassword: func(context.Context, string) ([]byte, error) {
			return []byte(strings.TrimSpace(input)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func providerLoginInvocation(providerID string, options map[string]any) Invocation {
	return Invocation{Operation: OpProvidersLogin, Arguments: []string{providerID}, Options: options, Yes: true}
}

func TestProviderLoginAppliesProtectedAPIKeyFromStdinAndFile(t *testing.T) {
	const canary = "sk-provider-router-canary-1234567890"
	for _, test := range []struct {
		name    string
		options map[string]any
		input   string
		prepare func(*testing.T, map[string]any)
	}{
		{name: "stdin", options: map[string]any{"api_key_stdin": true}, input: canary + "\n"},
		{name: "private file", options: map[string]any{}, prepare: func(t *testing.T, options map[string]any) {
			path := t.TempDir() + "/provider.key"
			if err := os.WriteFile(path, []byte(canary+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			options["api_key_file"] = path
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowAPIKey}}
			if test.prepare != nil {
				test.prepare(t, test.options)
			}
			result, err := providerRouter(t, fake, test.input).Execute(context.Background(), providerLoginInvocation("gemini", test.options), nil)
			if err != nil {
				t.Fatal(err)
			}
			if fake.apiKey != canary {
				t.Fatalf("protected input was not delivered: %q", fake.apiKey)
			}
			encoded := toJSON(result.Data)
			if strings.Contains(string(encoded), canary) {
				t.Fatalf("result disclosed API key: %s", encoded)
			}
		})
	}
}
func TestProviderLoginPreservesSelectedCompatibilityFields(t *testing.T) {
	const canary = "sk-provider-fields-canary-1234567890"
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowAPIKey}}
	fields := map[string]string{"base-url": "https://gateway.example/v1", "model-prefix": "team"}
	options := map[string]any{
		"api_key_stdin":     true,
		"provider_entry_id": "custom-team",
		"provider_label":    "Team gateway",
		"provider_fields":   fields,
	}
	result, err := providerRouter(t, fake, canary+"\n").Execute(
		context.Background(),
		providerLoginInvocation("openai-compatible", options),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if fake.application.ID != "custom-team" || fake.application.Label != "Team gateway" {
		t.Fatalf("selected provider entry was lost: %#v", fake.application)
	}
	if !reflect.DeepEqual(fake.application.Fields, fields) {
		t.Fatalf("compatibility fields = %#v, want %#v", fake.application.Fields, fields)
	}
	fields["base-url"] = "https://mutated.example"
	if fake.application.Fields["base-url"] != "https://gateway.example/v1" {
		t.Fatal("router retained the caller's mutable provider fields map")
	}
	encoded := toJSON(result.Data)
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("result disclosed API key: %s", encoded)
	}
}

func TestProviderLoginUsesEphemeralProtectedInputOverride(t *testing.T) {
	const canary = "sk-provider-tui-canary-1234567890"
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowAPIKey}}
	options := map[string]any{
		"api_key_stdin":   true,
		"protected_input": strings.NewReader(canary + "\n"),
	}
	result, err := providerRouter(t, fake, "wrong-shared-stdin\n").Execute(
		context.Background(),
		providerLoginInvocation("gemini", options),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if fake.apiKey != canary {
		t.Fatalf("ephemeral protected input was not delivered: %q", fake.apiKey)
	}
	encoded := toJSON(result.Data)
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("result disclosed protected input: %s", encoded)
	}
}

func TestProviderLoginRejectsNonPrivateAPIKeyFile(t *testing.T) {
	path := t.TempDir() + "/public-provider.key"
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowAPIKey}}
	_, err := providerRouter(t, fake, "").Execute(context.Background(), providerLoginInvocation("gemini", map[string]any{"api_key_file": path}), nil)
	if err == nil || fake.apiKey != "" {
		t.Fatalf("public API-key file was accepted: key=%q err=%v", fake.apiKey, err)
	}
}

func TestProviderLoginImportsVertexServiceAccountAndPrefix(t *testing.T) {
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowVertexImport}}
	options := map[string]any{"service_account": "/private/vertex.json", "vertex_prefix": "work"}
	result, err := providerRouter(t, fake, "").Execute(context.Background(), providerLoginInvocation("vertex", options), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.vertex.Path != "/private/vertex.json" || fake.vertex.Prefix != "work" {
		t.Fatalf("Vertex import request = %#v", fake.vertex)
	}
	if strings.Contains(toJSON(result.Data), "/private/vertex.json") {
		t.Fatal("Vertex service-account path was unnecessarily echoed")
	}
}

func TestProviderLoginCompletesProtectedPastedCallback(t *testing.T) {
	const callbackCanary = "callback-code-router-canary"
	callback := "http://127.0.0.1:54545/callback?state=s&code=" + callbackCanary
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowBrowser}}
	var events []Event
	result, err := providerRouter(t, fake, callback+"\n").Execute(context.Background(), providerLoginInvocation("claude", map[string]any{"callback_url_stdin": true, "method": "browser", "no_browser": true}), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.beginFlow != provider.FlowPasteCallback || fake.paste != callback || fake.polls != 0 {
		t.Fatalf("paste dispatch: begin=%q paste=%q polls=%d", fake.beginFlow, fake.paste, fake.polls)
	}
	encoded := toJSON(map[string]any{"data": result.Data, "human": result.Human, "events": events})
	if strings.Contains(string(encoded), callbackCanary) || strings.Contains(string(encoded), "oauth-state-must-not-render") {
		t.Fatalf("callback secret or OAuth state leaked: %s", encoded)
	}
}

func TestProviderLoginDispatchesBrowserAndDeviceFlows(t *testing.T) {
	for _, test := range []struct {
		name      string
		method    string
		noBrowser bool
		want      provider.AuthFlow
	}{
		{name: "browser", method: "browser", want: provider.FlowBrowser},
		{name: "headless browser", method: "browser", noBrowser: true, want: provider.FlowPasteCallback},
		{name: "device", method: "device", want: provider.FlowDeviceCode},
		{name: "auto", method: "auto", want: provider.FlowDeviceCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &providerFlowFake{
				flows:        []provider.AuthFlow{provider.FlowBrowser, provider.FlowDeviceCode},
				pollStatuses: []management.OAuthStatus{{Status: "pending"}, {Status: "complete"}},
			}
			var events []Event
			_, err := providerRouter(t, fake, "").Execute(context.Background(), providerLoginInvocation("codex", map[string]any{"method": test.method, "no_browser": test.noBrowser}), func(event Event) error {
				events = append(events, event)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if fake.beginFlow != test.want || fake.polls != 2 {
				t.Fatalf("begin=%q polls=%d want=%q/2", fake.beginFlow, fake.polls, test.want)
			}
			if len(events) < 3 || events[0].Type != "auth_started" || events[len(events)-1].Type != "complete" {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestProviderLoginAutoAPIKeyUsesProtectedTTYWithoutOAuthBegin(t *testing.T) {
	const canary = "sk-interactive-api-key-canary-123456"
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowAPIKey}}
	invocation := providerLoginInvocation("gemini", nil)
	invocation.Interactive = true
	invocation.Yes = false
	result, err := providerRouter(t, fake, canary).Execute(context.Background(), invocation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.beginFlow != "" || fake.apiKey != canary {
		t.Fatalf("auto API-key route used OAuth or wrong protected input: begin=%q key=%q", fake.beginFlow, fake.apiKey)
	}
	encoded := toJSON(result.Data)
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("interactive API key leaked in result: %s", encoded)
	}
}

func TestProviderLoginInteractiveCallbackPromptsAfterChallenge(t *testing.T) {
	const callbackCanary = "interactive-callback-canary"
	callback := "http://127.0.0.1:54545/callback?state=s&code=" + callbackCanary
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowBrowser}}
	invocation := providerLoginInvocation("claude", map[string]any{"method": "browser"})
	invocation.Interactive = true
	invocation.Yes = false
	var events []Event
	result, err := providerRouter(t, fake, callback).Execute(context.Background(), invocation, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.beginFlow != provider.FlowBrowser || fake.paste != callback || fake.polls != 0 {
		t.Fatalf("interactive callback dispatch: begin=%q paste=%q polls=%d", fake.beginFlow, fake.paste, fake.polls)
	}
	if len(events) < 4 || events[1].Type != "verification_required" || events[2].Type != "protected_input_required" {
		t.Fatalf("callback progress events = %#v", events)
	}
	encoded := toJSON(map[string]any{"data": result.Data, "human": result.Human, "events": events})
	if strings.Contains(string(encoded), callbackCanary) || strings.Contains(string(encoded), "oauth-state-must-not-render") {
		t.Fatalf("interactive callback secret or OAuth state leaked: %s", encoded)
	}
}

func TestProviderLoginBareNoninteractiveAPIRequiresProtectedInputFlag(t *testing.T) {
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowAPIKey}}
	_, err := providerRouter(t, fake, "ignored").Execute(context.Background(), providerLoginInvocation("gemini", nil), nil)
	if err == nil || fake.beginFlow != "" || fake.apiKey != "" {
		t.Fatalf("bare noninteractive API login reached authenticator: err=%v fake=%#v", err, fake)
	}
}

func TestProviderLoginRejectsInvalidRouteCombinations(t *testing.T) {
	for _, test := range []struct {
		name    string
		options map[string]any
		yes     bool
	}{
		{name: "two API inputs", options: map[string]any{"api_key_file": "/private/key", "api_key_stdin": true}, yes: true},
		{name: "API and Vertex", options: map[string]any{"api_key_stdin": true, "service_account": "/private/vertex.json"}, yes: true},
		{name: "prefix without service account", options: map[string]any{"vertex_prefix": "work"}, yes: true},
		{name: "callback with device", options: map[string]any{"callback_url_stdin": true, "method": "device"}, yes: true},
		{name: "API with browser method", options: map[string]any{"api_key_stdin": true, "method": "browser"}, yes: true},
		{name: "Vertex with no browser", options: map[string]any{"service_account": "/private/vertex.json", "no_browser": true}, yes: true},
		{name: "noninteractive API without confirmation", options: map[string]any{"api_key_stdin": true}},
		{name: "noninteractive Vertex without confirmation", options: map[string]any{"service_account": "/private/vertex.json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowBrowser, provider.FlowDeviceCode, provider.FlowAPIKey, provider.FlowVertexImport}}
			invocation := providerLoginInvocation("fixture", test.options)
			invocation.Yes = test.yes
			_, err := providerRouter(t, fake, "secret").Execute(context.Background(), invocation, nil)
			if err == nil {
				t.Fatal("invalid provider login combination was accepted")
			}
			if fake.beginFlow != "" || fake.apiKey != "" || fake.vertex.Path != "" {
				t.Fatalf("invalid input reached authenticator: %#v", fake)
			}
		})
	}
}

func TestProvidersVerifyEnforcesUsableAccountsAndRefreshesModels(t *testing.T) {
	newRouter := func(files []management.AuthFile, catalog *providerVerifyCatalog) *Router {
		t.Helper()
		installation := state.Installation{ID: "default", Kind: "managed"}
		router, err := NewRouter(Dependencies{
			Roots: testRoots(),
			Store: configuredState(installation),
			Management: func(context.Context, state.Installation) (management.ManagementClient, error) {
				return &providerVerifyManagement{files: files}, nil
			},
			Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) {
				return catalog, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return router
	}

	t.Run("single disabled provider is auth failure", func(t *testing.T) {
		result, err := newRouter([]management.AuthFile{{Name: "codex-disabled", Provider: "codex", Disabled: true, Status: "disabled"}}, &providerVerifyCatalog{}).
			Execute(context.Background(), Invocation{Operation: OpProvidersVerify, Arguments: []string{"codex"}}, nil)
		if err == nil {
			t.Fatal("disabled target was reported usable")
		}
		data := result.Data.(map[string]any)
		if data["usable"] != 0 || data["total"] != 1 {
			t.Fatalf("targeted verification data = %#v", data)
		}
	})

	t.Run("mixed all-provider result is unhealthy and retains every record", func(t *testing.T) {
		files := []management.AuthFile{
			{Name: "codex-ok", Provider: "codex", Status: "authenticated"},
			{Name: "claude-expired", Provider: "claude", Status: "expired"},
		}
		result, err := newRouter(files, &providerVerifyCatalog{}).
			Execute(context.Background(), Invocation{Operation: OpProvidersVerify}, nil)
		var typed *pmuxerr.Error
		if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUnhealthy {
			t.Fatalf("mixed verification error = %#v", err)
		}
		data := result.Data.(map[string]any)
		if data["usable"] != 1 || data["total"] != 2 || len(data["accounts"].([]management.AuthFile)) != 2 {
			t.Fatalf("mixed verification data = %#v", data)
		}
	})

	t.Run("explicit all alias retains every record", func(t *testing.T) {
		files := []management.AuthFile{
			{Name: "codex-ok", Provider: "codex", Status: "authenticated"},
			{Name: "kimi-ok", Provider: "kimi", Status: "usable"},
		}
		result, err := newRouter(files, &providerVerifyCatalog{}).
			Execute(context.Background(), Invocation{Operation: OpProvidersVerify, Arguments: []string{"all"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		data := result.Data.(map[string]any)
		if data["usable"] != 2 || data["total"] != 2 || len(data["accounts"].([]management.AuthFile)) != 2 {
			t.Fatalf("all-provider verification data = %#v", data)
		}
	})

	t.Run("refresh models uses live catalog without a model test", func(t *testing.T) {
		catalog := &providerVerifyCatalog{entries: []domainmodel.CatalogEntry{{ID: "live-model", Available: true}}}
		result, err := newRouter([]management.AuthFile{{Name: "kimi-ok", Provider: "kimi", Status: "authenticated"}}, catalog).
			Execute(context.Background(), Invocation{Operation: OpProvidersVerify, Options: map[string]any{"refresh_models": true}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if catalog.refreshes != 1 {
			t.Fatalf("catalog refresh calls = %d", catalog.refreshes)
		}
		data := result.Data.(map[string]any)
		refresh := data["model_refresh"].(map[string]any)
		if refresh["count"] != 1 || refresh["status"] != "complete" {
			t.Fatalf("model refresh result = %#v", refresh)
		}
	})
}

func TestProviderMutationsRequireScopeSpecificInteractivePhrases(t *testing.T) {
	for _, test := range []struct {
		name       string
		operation  Operation
		arguments  []string
		input      string
		wantPatch  int
		wantDelete int
		wantError  bool
	}{
		{name: "disable rejected", operation: OpProvidersDisable, arguments: []string{"codex"}, input: "wrong\n", wantError: true},
		{name: "disable confirmed", operation: OpProvidersDisable, arguments: []string{"codex"}, input: "disable\n", wantPatch: 1},
		{name: "provider removal confirmed by provider ID", operation: OpProvidersRemove, arguments: []string{"codex"}, input: "codex\n", wantDelete: 1},
		{name: "account removal confirmed", operation: OpProvidersRemove, arguments: []string{"codex", "codex-account"}, input: "remove\n", wantDelete: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			installation := state.Installation{ID: "default", Kind: "managed"}
			client := &providerVerifyManagement{files: []management.AuthFile{{Name: "codex-account", Provider: "codex", Status: "authenticated"}}}
			router, err := NewRouter(Dependencies{
				Roots: testRoots(),
				Store: configuredState(installation),
				Input: strings.NewReader(test.input),
				Management: func(context.Context, state.Installation) (management.ManagementClient, error) {
					return client, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = router.Execute(context.Background(), Invocation{
				Operation:   test.operation,
				Arguments:   test.arguments,
				Interactive: true,
			}, nil)
			if (err != nil) != test.wantError {
				t.Fatalf("mutation error = %v, wantError=%t", err, test.wantError)
			}
			if client.patches != test.wantPatch || len(client.deletes) != test.wantDelete {
				t.Fatalf("mutation calls patches=%d deletes=%#v", client.patches, client.deletes)
			}
		})
	}
}

func TestProviderLoginCancelsSessionAfterPollingCancellation(t *testing.T) {
	canceled := pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Authentication was canceled; no active session remains.")
	fake := &providerFlowFake{flows: []provider.AuthFlow{provider.FlowBrowser}, pollErr: canceled}
	_, err := providerRouter(t, fake, "").Execute(context.Background(), providerLoginInvocation("claude", map[string]any{"method": "browser"}), nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeCanceled || fake.canceled != 1 {
		t.Fatalf("poll error=%#v cancel calls=%d", err, fake.canceled)
	}
}
