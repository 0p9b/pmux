package management

import (
	"context"
	"encoding/json"
	"time"
)

type ProviderID string
type ProviderKeyKind string
type SettingName string

type CoreInfo struct {
	Version string `json:"version"`
	Healthy bool   `json:"healthy"`
	Warning string `json:"warning,omitempty"`
}
type ModelRef struct {
	ID        string `json:"id"`
	Owner     string `json:"owner,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Available bool   `json:"available"`
}
type CapabilitySet map[string]bool
type ConfigView map[string]any
type SettingValue json.RawMessage
type SettingPatch json.RawMessage
type SecretRef struct {
	Mask        string `json:"mask"`
	Fingerprint string `json:"fingerprint"`
}
type SecretValue string
type KeyPatch json.RawMessage
type APIKeyUsage struct {
	Fingerprint string `json:"fingerprint"`
	Requests    int64  `json:"requests"`
}
type ProviderKey struct {
	ID     string            `json:"id"`
	Label  string            `json:"label,omitempty"`
	Mask   string            `json:"mask,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}
type ProviderKeyPatch json.RawMessage
type AuthFile struct {
	Name     string     `json:"name"`
	Provider ProviderID `json:"provider"`
	Disabled bool       `json:"disabled"`
	Status   string     `json:"status,omitempty"`
}
type ModelDef struct {
	ID    string `json:"id"`
	Owner string `json:"owner,omitempty"`
}
type AuthFileStatusPatch json.RawMessage
type AuthFileFieldsPatch json.RawMessage
type ExcludedModelSet map[string][]string
type ExcludedModelPatch json.RawMessage
type ModelAliasSet map[string]map[string]string
type ModelAliasPatch json.RawMessage
type OAuthChallenge struct {
	State           string        `json:"state"`
	URL             string        `json:"url,omitempty"`
	VerificationURI string        `json:"verification_uri,omitempty"`
	UserCode        string        `json:"user_code,omitempty"`
	ExpiresAt       time.Time     `json:"expires_at,omitempty"`
	Interval        time.Duration `json:"interval,omitempty"`
}
type OAuthStatus struct {
	State   string `json:"state"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
type LogQuery struct {
	Level string
	Since time.Time
	Tail  int
}
type LogRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}
type LogPage struct {
	Records []LogRecord `json:"records"`
	Next    string      `json:"next,omitempty"`
}
type RequestErrorLog struct {
	Name    string `json:"name"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}
type RequestLog struct {
	ID     string `json:"id"`
	Status int    `json:"status"`
	Method string `json:"method"`
	Path   string `json:"path"`
}
type UsageRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
}
type VertexImportRequest struct {
	Path   string
	Prefix string
}
type VertexImportResult struct {
	Name string `json:"name"`
}
type ResetQuotaRequest struct{ Name string }
type APICallRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}
type APICallResponse struct {
	Status  int
	Headers map[string][]string
	Body    []byte
}
type PluginConfigField struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	EnumValues  []string `json:"enum_values,omitempty"`
	Description string   `json:"description,omitempty"`
}
type PluginMenu struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description,omitempty"`
}
type PluginMetadata struct {
	Name             string `json:"name,omitempty"`
	Version          string `json:"version,omitempty"`
	Author           string `json:"author,omitempty"`
	GitHubRepository string `json:"github_repository,omitempty"`
	Logo             string `json:"logo,omitempty"`
}
type PluginInfo struct {
	ID               string              `json:"id"`
	Path             string              `json:"path,omitempty"`
	Configured       bool                `json:"configured"`
	Registered       bool                `json:"registered"`
	Enabled          bool                `json:"enabled"`
	EffectiveEnabled bool                `json:"effective_enabled"`
	SupportsOAuth    bool                `json:"supports_oauth"`
	OAuthProvider    string              `json:"oauth_provider,omitempty"`
	Logo             string              `json:"logo,omitempty"`
	ConfigFields     []PluginConfigField `json:"config_fields,omitempty"`
	Menus            []PluginMenu        `json:"menus,omitempty"`
	Metadata         PluginMetadata      `json:"metadata,omitempty"`
}
type PluginList struct {
	PluginsEnabled bool         `json:"plugins_enabled"`
	PluginsDir     string       `json:"plugins_dir"`
	Plugins        []PluginInfo `json:"plugins"`
}
type StoreSource struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}
type StoreSourceError struct {
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name,omitempty"`
	SourceURL  string `json:"source_url,omitempty"`
	Message    string `json:"message,omitempty"`
}
type StorePluginPlatform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}
type StorePlugin struct {
	StoreID          string                `json:"store_id,omitempty"`
	SourceID         string                `json:"source_id,omitempty"`
	SourceName       string                `json:"source_name,omitempty"`
	ID               string                `json:"id"`
	Name             string                `json:"name,omitempty"`
	Description      string                `json:"description,omitempty"`
	Author           string                `json:"author,omitempty"`
	Version          string                `json:"version,omitempty"`
	Repository       string                `json:"repository,omitempty"`
	InstallType      string                `json:"install_type,omitempty"`
	AuthRequired     bool                  `json:"auth_required"`
	AuthConfigured   bool                  `json:"auth_configured"`
	Platforms        []StorePluginPlatform `json:"platforms,omitempty"`
	Homepage         string                `json:"homepage,omitempty"`
	License          string                `json:"license,omitempty"`
	Tags             []string              `json:"tags,omitempty"`
	Installed        bool                  `json:"installed"`
	InstalledVersion string                `json:"installed_version,omitempty"`
	UpdateAvailable  bool                  `json:"update_available"`
}
type PluginStoreList struct {
	PluginsEnabled bool               `json:"plugins_enabled"`
	PluginsDir     string             `json:"plugins_dir"`
	Sources        []StoreSource      `json:"sources"`
	SourceErrors   []StoreSourceError `json:"source_errors,omitempty"`
	Plugins        []StorePlugin      `json:"plugins"`
}
type PluginInstallResult struct {
	Status          string `json:"status"`
	ID              string `json:"id"`
	Version         string `json:"version,omitempty"`
	Path            string `json:"path,omitempty"`
	PluginsEnabled  bool   `json:"plugins_enabled"`
	RestartRequired bool   `json:"restart_required"`
}
type PluginDeleteResult struct {
	ID              string `json:"id"`
	Path            string `json:"path,omitempty"`
	FileDeleted     bool   `json:"file_deleted"`
	ConfigRemoved   bool   `json:"configured_removed"`
	RestartRequired bool   `json:"restart_required"`
}

const (
	ProviderGemini           ProviderKeyKind = "gemini-api-key"
	ProviderInteractions     ProviderKeyKind = "interactions-api-key"
	ProviderClaude           ProviderKeyKind = "claude-api-key"
	ProviderCodex            ProviderKeyKind = "codex-api-key"
	ProviderXAI              ProviderKeyKind = "xai-api-key"
	ProviderVertex           ProviderKeyKind = "vertex-api-key"
	ProviderOpenAICompatible ProviderKeyKind = "openai-compatibility"
)

type ManagementClient interface {
	Health(context.Context) (CoreInfo, error)
	PublicModels(context.Context) ([]ModelRef, error)
	Capabilities(context.Context) (CapabilitySet, error)
	Config(context.Context) (ConfigView, error)
	ConfigYAML(context.Context) ([]byte, error)
	PutConfigYAML(context.Context, []byte) error
	GetSetting(context.Context, SettingName) (SettingValue, error)
	PutSetting(context.Context, SettingName, SettingValue) error
	PatchSetting(context.Context, SettingName, SettingPatch) error
	DeleteSetting(context.Context, SettingName) error
	APIKeys(context.Context) ([]SecretRef, error)
	PutAPIKeys(context.Context, []SecretValue) error
	PatchAPIKeys(context.Context, KeyPatch) error
	DeleteAPIKey(context.Context, string) error
	APIKeyUsage(context.Context) ([]APIKeyUsage, error)
	ProviderKeys(context.Context, ProviderKeyKind) ([]ProviderKey, error)
	PutProviderKeys(context.Context, ProviderKeyKind, []ProviderKey) error
	PatchProviderKeys(context.Context, ProviderKeyKind, ProviderKeyPatch) error
	DeleteProviderKey(context.Context, ProviderKeyKind, string) error
	AuthFiles(context.Context) ([]AuthFile, error)
	AuthFileModels(context.Context, string) ([]ModelRef, error)
	ModelDefinitions(context.Context, string) ([]ModelDef, error)
	DeleteAuthFiles(context.Context, []string, bool) error
	PatchAuthFileStatus(context.Context, AuthFileStatusPatch) error
	PatchAuthFileFields(context.Context, AuthFileFieldsPatch) error
	ExcludedModels(context.Context) (ExcludedModelSet, error)
	PutExcludedModels(context.Context, ExcludedModelSet) error
	PatchExcludedModels(context.Context, ExcludedModelPatch) error
	DeleteExcludedModels(context.Context, string) error
	ModelAliases(context.Context) (ModelAliasSet, error)
	PutModelAliases(context.Context, ModelAliasSet) error
	PatchModelAliases(context.Context, ModelAliasPatch) error
	DeleteModelAliases(context.Context, string) error
	BeginOAuth(context.Context, ProviderID, bool) (OAuthChallenge, error)
	OAuthStatus(context.Context, string) (OAuthStatus, error)
	SubmitOAuthCallback(context.Context, string) error
	CancelOAuth(context.Context, string) error
	Logs(context.Context, LogQuery) (LogPage, error)
	DeleteLogs(context.Context) error
	RequestErrorLogs(context.Context) ([]RequestErrorLog, error)
	RequestErrorLog(context.Context, string) (RequestErrorLog, error)
	RequestLogByID(context.Context, string) (RequestLog, error)
	PopUsageQueue(context.Context) ([]UsageRecord, error)
	ImportVertex(context.Context, VertexImportRequest) (VertexImportResult, error)
	ResetQuota(context.Context, ResetQuotaRequest) error
	LatestVersion(context.Context) (string, error)
	APICall(context.Context, APICallRequest) (APICallResponse, error)
	Plugins(context.Context) (PluginList, error)
	PluginStore(context.Context) (PluginStoreList, error)
	InstallPlugin(context.Context, string, string, string) (PluginInstallResult, error)
	SetPluginEnabled(context.Context, string, bool) error
	PluginConfig(context.Context, string) (map[string]any, error)
	PutPluginConfig(context.Context, string, map[string]any) error
	PatchPluginConfig(context.Context, string, map[string]any) error
	DeletePlugin(context.Context, string) (PluginDeleteResult, error)
}
