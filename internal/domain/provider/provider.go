package provider

import (
	"context"

	"github.com/0p9b/pmux/internal/domain/management"
)

type AuthFlow string

const (
	FlowBrowser       AuthFlow = "browser"
	FlowPasteCallback AuthFlow = "paste_callback"
	FlowDeviceCode    AuthFlow = "device_code"
	FlowAPIKey        AuthFlow = "api_key"
	FlowVertexImport  AuthFlow = "vertex_import"
	FlowSubprocess    AuthFlow = "subprocess"
)

type AuthSession struct {
	Provider  management.ProviderID
	Flow      AuthFlow
	Challenge management.OAuthChallenge
}

// ProtectedInput supplies a secret from a non-argv source. Implementations
// must not echo or retain the returned bytes.
type ProtectedInput interface {
	ReadSecret(context.Context) ([]byte, error)
}

type APIKeyApplication struct {
	Input  ProtectedInput
	ID     string
	Label  string
	Fields map[string]string
}

type VertexImport struct {
	Path   string
	Prefix string
}

type ProviderAuthenticator interface {
	Provider() management.ProviderID
	Flows() []AuthFlow
	Begin(context.Context, AuthFlow) (AuthSession, error)
	Poll(context.Context, AuthSession) (management.OAuthStatus, error)
	CompletePaste(context.Context, AuthSession, string) (management.OAuthStatus, error)
	ApplyAPIKey(context.Context, APIKeyApplication) (management.ProviderKey, error)
	ImportVertex(context.Context, VertexImport) (management.VertexImportResult, error)
	Cancel(context.Context, AuthSession) error
}

type Definition struct {
	ID              management.ProviderID
	Name            string
	Flows           []AuthFlow
	SubprocessFlags map[AuthFlow][]string
}

func Registry() []Definition {
	return []Definition{
		{ID: "codex", Name: "Codex", Flows: []AuthFlow{FlowBrowser, FlowDeviceCode, FlowAPIKey}, SubprocessFlags: map[AuthFlow][]string{FlowBrowser: {"-codex-login"}, FlowDeviceCode: {"-codex-device-login"}}},
		{ID: "codex-compatible", Name: "Codex-compatible", Flows: []AuthFlow{FlowAPIKey}},
		{ID: "claude", Name: "Claude", Flows: []AuthFlow{FlowBrowser, FlowAPIKey}, SubprocessFlags: map[AuthFlow][]string{FlowBrowser: {"-claude-login"}}},
		{ID: "claude-compatible", Name: "Claude-compatible", Flows: []AuthFlow{FlowAPIKey}},
		{ID: "antigravity", Name: "Antigravity", Flows: []AuthFlow{FlowBrowser}, SubprocessFlags: map[AuthFlow][]string{FlowBrowser: {"-antigravity-login"}}},
		{ID: "kimi", Name: "Kimi", Flows: []AuthFlow{FlowDeviceCode}, SubprocessFlags: map[AuthFlow][]string{FlowDeviceCode: {"-kimi-login"}}},
		{ID: "xai", Name: "xAI", Flows: []AuthFlow{FlowDeviceCode, FlowAPIKey}, SubprocessFlags: map[AuthFlow][]string{FlowDeviceCode: {"-xai-login"}}},
		{ID: "gemini", Name: "Gemini", Flows: []AuthFlow{FlowAPIKey}},
		{ID: "interactions", Name: "Interactions", Flows: []AuthFlow{FlowAPIKey}},
		{ID: "vertex", Name: "Vertex", Flows: []AuthFlow{FlowVertexImport, FlowAPIKey}, SubprocessFlags: map[AuthFlow][]string{FlowVertexImport: {"-vertex-import"}}},
		{ID: "openrouter", Name: "OpenRouter", Flows: []AuthFlow{FlowAPIKey}},
		{ID: "openai-compatible", Name: "OpenAI-compatible", Flows: []AuthFlow{FlowAPIKey}},
	}
}
