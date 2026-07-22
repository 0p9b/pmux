package mgmtapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

func (c *Client) Health(ctx context.Context) (management.CoreInfo, error) {
	body, headers, _, err := c.request(ctx, requestSpec{method: http.MethodGet, url: c.endpoint("healthz"), auth: authNone})
	if err != nil {
		return management.CoreInfo{}, err
	}
	var payload struct {
		Status string `json:"status"`
	}
	if len(bytes.TrimSpace(body)) != 0 {
		if err := decodeJSON(body, &payload); err != nil {
			return management.CoreInfo{}, err
		}
	}
	version := headers.Get("X-CPA-VERSION")
	info := management.CoreInfo{Version: version, Healthy: payload.Status == "" || strings.EqualFold(payload.Status, "ok") || strings.EqualFold(payload.Status, "healthy")}
	if version == "" {
		info.Version = "unknown"
		info.Warning = "CLIProxyAPI is healthy, but X-CPA-VERSION was not returned; version is unknown."
	}
	return info, nil
}

func (c *Client) PublicModels(ctx context.Context) ([]management.ModelRef, error) {
	body, _, _, err := c.request(ctx, requestSpec{method: http.MethodGet, url: c.endpoint("v1", "models"), auth: authProxy})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Data []struct {
			ID    string `json:"id"`
			Owner string `json:"owned_by"`
		} `json:"data"`
	}
	if err := decodeJSON(body, &envelope); err != nil {
		return nil, err
	}
	models := make([]management.ModelRef, 0, len(envelope.Data))
	for _, model := range envelope.Data {
		if model.ID == "" {
			continue
		}
		models = append(models, management.ModelRef{ID: model.ID, Owner: c.redact(model.Owner), Available: true})
	}
	return models, nil
}

func (c *Client) Capabilities(ctx context.Context) (management.CapabilitySet, error) {
	capabilities := management.CapabilitySet{}
	if _, err := c.Health(ctx); err == nil {
		capabilities["healthz"] = true
	}
	if c.proxyKey != "" {
		_, err := c.PublicModels(ctx)
		capabilities["public-models"] = err == nil
	}
	enabled, err := c.managementEnabled(ctx)
	if err != nil {
		return nil, err
	}
	capabilities["management"] = enabled
	families := map[string][]probeSpec{
		"management-config-yaml":       {{http.MethodGet, "config.yaml"}},
		"management-api-keys":          {{http.MethodGet, "api-keys"}},
		"management-provider-keys":     {{http.MethodGet, string(management.ProviderGemini)}},
		"management-oauth":             {{http.MethodOptions, "codex-auth-url"}, {http.MethodGet, "get-auth-status"}},
		"management-auth-files":        {{http.MethodGet, "auth-files"}},
		"management-model-attribution": {{http.MethodGet, "auth-files/models"}},
		"management-logs":              {{http.MethodGet, "logs"}},
		"management-vertex-import":     {{http.MethodOptions, "vertex/import"}},
		"management-reset-quota":       {{http.MethodOptions, "reset-quota"}},
		"management-latest-version":    {{http.MethodGet, "latest-version"}},
		"management-api-call":          {{http.MethodOptions, "api-call"}},
	}
	if !enabled {
		for feature := range families {
			capabilities[feature] = false
		}
		return capabilities, nil
	}
	for feature, probes := range families {
		available := true
		for _, probe := range probes {
			ok, probeErr := c.probe(ctx, probe)
			if probeErr != nil {
				return nil, probeErr
			}
			available = available && ok
		}
		capabilities[feature] = available
	}
	return capabilities, nil
}

type probeSpec struct{ method, endpoint string }

func (c *Client) probe(ctx context.Context, probe probeSpec) (bool, error) {
	parts := strings.Split(probe.endpoint, "/")
	_, _, status, err := c.request(ctx, requestSpec{method: probe.method, url: c.managementEndpoint(parts...), auth: authManagement, management: false})
	if status == http.StatusNotFound {
		return false, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return false, c.statusError(ctx, requestSpec{management: true}, status)
	}
	// A safe probe can legitimately receive 400 (missing operation input) or
	// 405 (the route exists but does not implement OPTIONS). Both prove the
	// endpoint family exists without performing a mutation.
	if status > 0 && status < 500 {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) Config(ctx context.Context) (management.ConfigView, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "config", nil)
	if err != nil {
		return nil, err
	}
	var config management.ConfigView
	if err := decodeJSON(body, &config); err != nil {
		return nil, err
	}
	redactMap(c, map[string]any(config))
	return config, nil
}

func (c *Client) ConfigYAML(ctx context.Context) ([]byte, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "config.yaml", nil)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// PutConfigYAML is a compensating transaction: it snapshots the prior payload,
// writes the candidate, verifies exact bytes, and restores the snapshot if the
// candidate is not observable. No write is retried after an authentication error.
func (c *Client) PutConfigYAML(ctx context.Context, candidate []byte) error {
	prior, err := c.ConfigYAML(ctx)
	if err != nil {
		return err
	}
	if _, _, _, err := c.managementRequest(ctx, http.MethodPut, "config.yaml", candidate); err != nil {
		return err
	}
	observed, verifyErr := c.ConfigYAML(ctx)
	if verifyErr == nil && bytes.Equal(observed, candidate) {
		return nil
	}
	if isManagementAuthError(verifyErr) {
		return verifyErr
	}
	restoreErr := c.restoreConfig(ctx, prior)
	if restoreErr != nil {
		return &pmuxerr.Error{Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Upstream, Message: "Config update verification failed, and automatic restore also failed; config may be partially changed.", Explanation: "The Management API did not return the requested config and the prior payload could not be verified after restoration.", Cause: restoreErr}
	}
	result := &pmuxerr.Error{Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Upstream, Message: "Config update verification failed; the prior config was restored.", Explanation: "The Management API accepted the write but did not return the requested payload."}
	if verifyErr != nil {
		result.Cause = verifyErr
	}
	return result
}

func (c *Client) restoreConfig(ctx context.Context, prior []byte) error {
	if _, _, _, err := c.managementRequest(ctx, http.MethodPut, "config.yaml", prior); err != nil {
		return err
	}
	observed, err := c.ConfigYAML(ctx)
	if err != nil {
		return err
	}
	if !bytes.Equal(observed, prior) {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Upstream, "The prior config could not be restored")
	}
	return nil
}

func isManagementAuthError(err error) bool {
	var typed *pmuxerr.Error
	return errors.As(err, &typed) && typed.Code == pmuxerr.ManagementAuthRejected
}

func (c *Client) GetSetting(ctx context.Context, name management.SettingName) (management.SettingValue, error) {
	endpoint, err := settingEndpoint(name)
	if err != nil {
		return nil, err
	}
	body, _, _, reqErr := c.managementRequest(ctx, http.MethodGet, endpoint, nil)
	if reqErr != nil {
		return nil, reqErr
	}
	if !json.Valid(body) {
		return nil, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "CLIProxyAPI returned malformed setting JSON")
	}
	return append(management.SettingValue(nil), body...), nil
}

func (c *Client) PutSetting(ctx context.Context, name management.SettingName, value management.SettingValue) error {
	return c.settingMutation(ctx, http.MethodPut, name, value)
}
func (c *Client) PatchSetting(ctx context.Context, name management.SettingName, patch management.SettingPatch) error {
	return c.settingMutation(ctx, http.MethodPatch, name, patch)
}
func (c *Client) DeleteSetting(ctx context.Context, name management.SettingName) error {
	return c.settingMutation(ctx, http.MethodDelete, name, nil)
}
func (c *Client) settingMutation(ctx context.Context, method string, name management.SettingName, body []byte) error {
	endpoint, err := settingEndpoint(name)
	if err != nil {
		return err
	}
	if body != nil {
		body, err = settingWriteBody(body)
		if err != nil {
			return err
		}
	}
	_, _, _, err = c.managementRequest(ctx, method, endpoint, body)
	return err
}

func settingWriteBody(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Setting value must be valid JSON")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) == nil {
		if _, ok := object["value"]; ok {
			return body, nil
		}
		if len(object) == 1 {
			for _, value := range object {
				return marshalBody(map[string]json.RawMessage{"value": value})
			}
		}
	}
	return marshalBody(map[string]json.RawMessage{"value": json.RawMessage(body)})
}

var settings = map[management.SettingName]string{
	"debug": "debug", "logging-to-file": "logging-to-file", "logs-max-total-size-mb": "logs-max-total-size-mb",
	"error-logs-max-files": "error-logs-max-files", "usage-statistics-enabled": "usage-statistics-enabled",
	"request-log": "request-log", "ws-auth": "ws-auth", "request-retry": "request-retry",
	"max-retry-interval": "max-retry-interval", "force-model-prefix": "force-model-prefix",
	"routing/strategy": "routing/strategy", "quota-exceeded/switch-project": "quota-exceeded/switch-project",
	"quota-exceeded/switch-preview-model": "quota-exceeded/switch-preview-model", "proxy-url": "proxy-url",
}

func settingEndpoint(name management.SettingName) (string, error) {
	endpoint, ok := settings[name]
	if !ok {
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("Unknown management setting %q", name))
	}
	return endpoint, nil
}

func (c *Client) APIKeys(ctx context.Context) ([]management.SecretRef, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "api-keys", nil)
	if err != nil {
		return nil, err
	}
	values, err := stringList(body, "api-keys", "keys")
	if err != nil {
		return nil, err
	}
	refs := make([]management.SecretRef, 0, len(values))
	for _, value := range values {
		refs = append(refs, management.SecretRef{Mask: redact.Mask(value), Fingerprint: fingerprint(value)})
	}
	return refs, nil
}
func (c *Client) PutAPIKeys(ctx context.Context, keys []management.SecretValue) error {
	values := make([]string, len(keys))
	for i := range keys {
		values[i] = string(keys[i])
	}
	return c.jsonMutation(ctx, http.MethodPut, "api-keys", values)
}
func (c *Client) PatchAPIKeys(ctx context.Context, patch management.KeyPatch) error {
	return c.rawMutation(ctx, http.MethodPatch, "api-keys", patch)
}
func (c *Client) DeleteAPIKey(ctx context.Context, wantedFingerprint string) error {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "api-keys", nil)
	if err != nil {
		return err
	}
	values, err := stringList(body, "api-keys", "keys")
	if err != nil {
		return err
	}
	for index, value := range values {
		if fingerprint(value) == wantedFingerprint {
			_, _, _, err = c.managementQuery(ctx, http.MethodDelete, "api-keys", queryWith("index", strconv.Itoa(index)), nil)
			return err
		}
	}
	return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "The selected proxy API key no longer exists")
}
func (c *Client) APIKeyUsage(ctx context.Context) ([]management.APIKeyUsage, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "api-key-usage", nil)
	if err != nil {
		return nil, err
	}
	var legacy struct {
		Data  []management.APIKeyUsage `json:"data"`
		Usage []management.APIKeyUsage `json:"usage"`
	}
	if json.Unmarshal(body, &legacy) == nil {
		if legacy.Data != nil {
			return legacy.Data, nil
		}
		if legacy.Usage != nil {
			return legacy.Usage, nil
		}
	}
	var providers map[string]map[string]struct {
		Success int64 `json:"success"`
		Failed  int64 `json:"failed"`
	}
	if err := decodeJSON(body, &providers); err != nil {
		return nil, err
	}
	result := make([]management.APIKeyUsage, 0)
	for _, entries := range providers {
		for composite, usage := range entries {
			key := composite
			if separator := strings.LastIndexByte(composite, '|'); separator >= 0 {
				key = composite[separator+1:]
			}
			result = append(result, management.APIKeyUsage{Fingerprint: fingerprint(key), Requests: usage.Success + usage.Failed})
		}
	}
	return result, nil
}

func (c *Client) ProviderKeys(ctx context.Context, kind management.ProviderKeyKind) ([]management.ProviderKey, error) {
	endpoint, err := providerEndpoint(kind)
	if err != nil {
		return nil, err
	}
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return nil, err
	}
	items, err := rawArray(raw, endpoint, "data", "items")
	if err != nil {
		return nil, err
	}
	values := make([]management.ProviderKey, 0, len(items))
	for _, item := range items {
		var value management.ProviderKey
		if err := decodeJSON(item, &value); err != nil {
			return nil, err
		}
		redactStringMap(c, value.Fields)
		value.Mask = c.redact(value.Mask)
		values = append(values, value)
	}
	return values, nil
}

// CreateProviderKey performs a lossless read/modify/write transaction against
// the upstream provider-key collection. It deliberately works with the raw
// collection because ProviderKeys returns redacted normalized records that
// cannot be used to restore existing credentials.
func (c *Client) CreateProviderKey(ctx context.Context, kind management.ProviderKeyKind, candidate management.ProviderKey) (management.ProviderKey, error) {
	endpoint, err := providerEndpoint(kind)
	if err != nil {
		return management.ProviderKey{}, err
	}
	prior, err := c.rawProviderKeys(ctx, endpoint)
	if err != nil {
		return management.ProviderKey{}, err
	}
	payload, secret, err := providerKeyPayload(kind, candidate)
	if err != nil {
		return management.ProviderKey{}, err
	}
	next := append(append([]json.RawMessage(nil), prior...), payload)
	if err := c.jsonMutation(ctx, http.MethodPut, endpoint, next); err != nil {
		if isManagementAuthError(err) {
			return management.ProviderKey{}, err
		}
		if restoreErr := c.restoreRawProviderKeys(ctx, endpoint, prior); restoreErr != nil {
			return management.ProviderKey{}, pmuxerr.Wrap(safeCause(restoreErr, "provider-key restore failed"), pmuxerr.ConfigMutationConflict, pmuxerr.Upstream, "The provider-key update failed and its prior collection could not be verified.")
		}
		return management.ProviderKey{}, err
	}
	observed, verifyErr := c.rawProviderKeys(ctx, endpoint)
	if verifyErr == nil && rawProviderKeyPresent(observed, payload) {
		return management.ProviderKey{ID: candidate.ID, Label: candidate.Label, Mask: redact.Mask(secret)}, nil
	}
	if isManagementAuthError(verifyErr) {
		return management.ProviderKey{}, verifyErr
	}
	if restoreErr := c.restoreRawProviderKeys(ctx, endpoint, prior); restoreErr != nil {
		return management.ProviderKey{}, pmuxerr.Wrap(safeCause(restoreErr, "provider-key restore failed"), pmuxerr.ConfigMutationConflict, pmuxerr.Upstream, "Provider-key verification failed and the prior collection could not be restored.")
	}
	if verifyErr != nil {
		return management.ProviderKey{}, verifyErr
	}
	return management.ProviderKey{}, pmuxerr.New(pmuxerr.OAuthNoUsableCredential, pmuxerr.Upstream, "Provider-key verification failed; the prior collection was restored.")
}

func (c *Client) PutProviderKeys(ctx context.Context, kind management.ProviderKeyKind, values []management.ProviderKey) error {
	endpoint, err := providerEndpoint(kind)
	if err != nil {
		return err
	}
	return c.jsonMutation(ctx, http.MethodPut, endpoint, values)
}
func (c *Client) PatchProviderKeys(ctx context.Context, kind management.ProviderKeyKind, patch management.ProviderKeyPatch) error {
	endpoint, err := providerEndpoint(kind)
	if err != nil {
		return err
	}
	return c.rawMutation(ctx, http.MethodPatch, endpoint, patch)
}
func (c *Client) DeleteProviderKey(ctx context.Context, kind management.ProviderKeyKind, id string) error {
	endpoint, err := providerEndpoint(kind)
	if err != nil {
		return err
	}
	return c.jsonMutation(ctx, http.MethodDelete, endpoint, map[string]string{"id": id})
}
func (c *Client) rawProviderKeys(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return nil, err
	}
	items, err := rawArray(raw, endpoint, "data", "items")
	if err != nil {
		return nil, err
	}
	return append([]json.RawMessage(nil), items...), nil
}

func providerKeyPayload(kind management.ProviderKeyKind, candidate management.ProviderKey) (json.RawMessage, string, error) {
	fields := make(map[string]any, len(candidate.Fields)+2)
	for key, value := range candidate.Fields {
		fields[key] = value
	}
	secret, _ := fields["api-key"].(string)
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "The provider API key is empty.")
	}
	if kind == management.ProviderOpenAICompatible {
		delete(fields, "api-key")
		fields["api-key-entries"] = []map[string]string{{"api-key": secret}}
		if strings.TrimSpace(candidate.Label) != "" {
			fields["name"] = strings.TrimSpace(candidate.Label)
		}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, "", pmuxerr.Wrap(safeCause(err, "provider-key encoding failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not prepare the provider-key entry.")
	}
	return payload, secret, nil
}

func rawProviderKeyPresent(items []json.RawMessage, expected json.RawMessage) bool {
	var expectedValue any
	if json.Unmarshal(expected, &expectedValue) != nil {
		return false
	}
	for _, item := range items {
		var observed any
		if json.Unmarshal(item, &observed) == nil && jsonContains(observed, expectedValue) {
			return true
		}
	}
	return false
}

func jsonContains(observed, expected any) bool {
	switch want := expected.(type) {
	case map[string]any:
		got, ok := observed.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range want {
			if !jsonContains(got[key], value) {
				return false
			}
		}
		return true
	case []any:
		got, ok := observed.([]any)
		if !ok || len(got) < len(want) {
			return false
		}
		for _, value := range want {
			found := false
			for _, candidate := range got {
				if jsonContains(candidate, value) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return observed == expected
	}
}

func (c *Client) restoreRawProviderKeys(ctx context.Context, endpoint string, prior []json.RawMessage) error {
	if err := c.jsonMutation(ctx, http.MethodPut, endpoint, prior); err != nil {
		return err
	}
	observed, err := c.rawProviderKeys(ctx, endpoint)
	if err != nil {
		return err
	}
	left, leftErr := canonicalJSON(prior)
	right, rightErr := canonicalJSON(observed)
	if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Upstream, "The restored provider-key collection did not match its prior snapshot.")
	}
	return nil
}

func canonicalJSON(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(body, &normalized); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func providerEndpoint(kind management.ProviderKeyKind) (string, error) {
	switch kind {
	case management.ProviderGemini, management.ProviderInteractions, management.ProviderClaude, management.ProviderCodex, management.ProviderXAI, management.ProviderVertex, management.ProviderOpenAICompatible:
		return string(kind), nil
	default:
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("Unknown provider key kind %q", kind))
	}
}

func (c *Client) AuthFiles(ctx context.Context) ([]management.AuthFile, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "auth-files", nil)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return nil, err
	}
	items, err := rawArray(raw, "auth-files", "files", "data")
	if err != nil {
		return nil, err
	}
	values := make([]management.AuthFile, 0, len(items))
	for _, item := range items {
		var value management.AuthFile
		if err := decodeJSON(item, &value); err != nil {
			return nil, err
		}
		value.Name = c.redact(value.Name)
		value.Status = c.redact(value.Status)
		values = append(values, value)
	}
	return values, nil
}
func (c *Client) AuthFileModels(ctx context.Context, name string) ([]management.ModelRef, error) {
	body, _, _, err := c.managementQuery(ctx, http.MethodGet, "auth-files/models", queryWith("name", name), nil)
	if err != nil {
		return nil, err
	}
	return decodeModelRefs(body, c)
}
func (c *Client) ModelDefinitions(ctx context.Context, channel string) ([]management.ModelDef, error) {
	body, _, _, err := c.managementRequestParts(ctx, http.MethodGet, []string{"model-definitions", channel}, nil, nil, "")
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return nil, err
	}
	items, err := rawArray(raw, "models", "data")
	if err != nil {
		return nil, err
	}
	values := make([]management.ModelDef, 0, len(items))
	for _, item := range items {
		var value management.ModelDef
		if err := decodeJSON(item, &value); err != nil {
			return nil, err
		}
		value.Owner = c.redact(value.Owner)
		values = append(values, value)
	}
	return values, nil
}
func (c *Client) DeleteAuthFiles(ctx context.Context, names []string, all bool) error {
	payload := map[string]any{"names": names}
	if all {
		payload["all"] = true
	}
	return c.jsonMutation(ctx, http.MethodDelete, "auth-files", payload)
}
func (c *Client) PatchAuthFileStatus(ctx context.Context, patch management.AuthFileStatusPatch) error {
	return c.rawMutation(ctx, http.MethodPatch, "auth-files/status", patch)
}
func (c *Client) PatchAuthFileFields(ctx context.Context, patch management.AuthFileFieldsPatch) error {
	return c.rawMutation(ctx, http.MethodPatch, "auth-files/fields", patch)
}

func (c *Client) ExcludedModels(ctx context.Context) (management.ExcludedModelSet, error) {
	var value management.ExcludedModelSet
	err := c.getJSON(ctx, "oauth-excluded-models", &value)
	return value, err
}
func (c *Client) PutExcludedModels(ctx context.Context, value management.ExcludedModelSet) error {
	return c.jsonMutation(ctx, http.MethodPut, "oauth-excluded-models", value)
}
func (c *Client) PatchExcludedModels(ctx context.Context, patch management.ExcludedModelPatch) error {
	return c.rawMutation(ctx, http.MethodPatch, "oauth-excluded-models", patch)
}
func (c *Client) DeleteExcludedModels(ctx context.Context, channel string) error {
	return c.jsonMutation(ctx, http.MethodDelete, "oauth-excluded-models", map[string]string{"channel": channel})
}
func (c *Client) ModelAliases(ctx context.Context) (management.ModelAliasSet, error) {
	var value management.ModelAliasSet
	err := c.getJSON(ctx, "oauth-model-alias", &value)
	return value, err
}
func (c *Client) PutModelAliases(ctx context.Context, value management.ModelAliasSet) error {
	return c.jsonMutation(ctx, http.MethodPut, "oauth-model-alias", value)
}
func (c *Client) PatchModelAliases(ctx context.Context, patch management.ModelAliasPatch) error {
	return c.rawMutation(ctx, http.MethodPatch, "oauth-model-alias", patch)
}
func (c *Client) DeleteModelAliases(ctx context.Context, channel string) error {
	return c.jsonMutation(ctx, http.MethodDelete, "oauth-model-alias", map[string]string{"channel": channel})
}

func (c *Client) BeginOAuth(ctx context.Context, provider management.ProviderID, webUI bool) (management.OAuthChallenge, error) {
	endpoint, err := oauthEndpoint(provider)
	if err != nil {
		return management.OAuthChallenge{}, err
	}
	query := make(url.Values)
	if webUI {
		query.Set("is_webui", "true")
	}
	body, _, _, err := c.managementQuery(ctx, http.MethodGet, endpoint, query, nil)
	if err != nil {
		return management.OAuthChallenge{}, err
	}
	var challenge management.OAuthChallenge
	if err := decodeJSON(body, &challenge); err != nil {
		return management.OAuthChallenge{}, err
	}
	challenge.URL = c.redact(challenge.URL)
	challenge.VerificationURI = c.redact(challenge.VerificationURI)
	return challenge, nil
}
func oauthEndpoint(provider management.ProviderID) (string, error) {
	switch strings.ToLower(string(provider)) {
	case "claude", "anthropic":
		return "anthropic-auth-url", nil
	case "codex":
		return "codex-auth-url", nil
	case "antigravity":
		return "antigravity-auth-url", nil
	case "kimi":
		return "kimi-auth-url", nil
	case "xai":
		return "xai-auth-url", nil
	default:
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("Provider %q has no Management API OAuth flow", provider))
	}
}
func (c *Client) OAuthStatus(ctx context.Context, state string) (management.OAuthStatus, error) {
	body, _, _, err := c.managementQuery(ctx, http.MethodGet, "get-auth-status", queryWith("state", state), nil)
	if err != nil {
		return management.OAuthStatus{}, err
	}
	var status management.OAuthStatus
	if err := decodeJSON(body, &status); err != nil {
		return status, err
	}
	status.Message = c.redact(status.Message)
	return status, nil
}
func (c *Client) SubmitOAuthCallback(ctx context.Context, callbackURL string) error {
	return c.jsonMutation(ctx, http.MethodPost, "oauth-callback", map[string]string{"callback_url": callbackURL})
}
func (c *Client) CancelOAuth(ctx context.Context, state string) error {
	return c.jsonMutation(ctx, http.MethodDelete, "oauth-session", map[string]string{"state": state})
}

func (c *Client) Logs(ctx context.Context, query management.LogQuery) (management.LogPage, error) {
	values := make(url.Values)
	if query.Level != "" {
		values.Set("level", query.Level)
	}
	if !query.Since.IsZero() {
		values.Set("after", strconv.FormatInt(query.Since.Unix(), 10))
	}
	if query.Tail > 0 {
		values.Set("limit", strconv.Itoa(query.Tail))
	}
	body, _, _, err := c.managementQuery(ctx, http.MethodGet, "logs", values, nil)
	if err != nil {
		return management.LogPage{}, err
	}
	var payload struct {
		Records    []management.LogRecord `json:"records"`
		Lines      []string               `json:"lines"`
		Next       string                 `json:"next"`
		NextCursor string                 `json:"next-cursor"`
	}
	if err := decodeJSON(body, &payload); err != nil {
		return management.LogPage{}, err
	}
	page := management.LogPage{Records: payload.Records, Next: payload.Next}
	if page.Next == "" {
		page.Next = payload.NextCursor
	}
	for _, line := range payload.Lines {
		page.Records = append(page.Records, management.LogRecord{Message: c.redact(line)})
	}
	for i := range page.Records {
		page.Records[i].Message = c.redact(page.Records[i].Message)
	}
	return page, nil
}
func (c *Client) DeleteLogs(ctx context.Context) error {
	_, _, _, err := c.managementRequest(ctx, http.MethodDelete, "logs", nil)
	return err
}
func (c *Client) RequestErrorLogs(ctx context.Context) ([]management.RequestErrorLog, error) {
	var values []management.RequestErrorLog
	err := c.getListJSON(ctx, "request-error-logs", &values, "files", "logs", "data")
	if err == nil {
		for i := range values {
			values[i].Name = c.redact(values[i].Name)
			values[i].Message = c.redact(values[i].Message)
		}
	}
	return values, err
}
func (c *Client) RequestErrorLog(ctx context.Context, name string) (management.RequestErrorLog, error) {
	var value management.RequestErrorLog
	body, _, _, err := c.managementRequestParts(ctx, http.MethodGet, []string{"request-error-logs", name}, nil, nil, "")
	if err == nil {
		err = decodeJSON(body, &value)
	}
	value.Name = c.redact(value.Name)
	value.Message = c.redact(value.Message)
	return value, err
}
func (c *Client) RequestLogByID(ctx context.Context, id string) (management.RequestLog, error) {
	var value management.RequestLog
	body, _, _, err := c.managementRequestParts(ctx, http.MethodGet, []string{"request-log-by-id", id}, nil, nil, "")
	if err == nil {
		err = decodeJSON(body, &value)
	}
	value.Path = c.redact(value.Path)
	return value, err
}
func (c *Client) PopUsageQueue(ctx context.Context) ([]management.UsageRecord, error) {
	var values []management.UsageRecord
	err := c.getListJSON(ctx, "usage-queue", &values, "records", "data")
	return values, err
}

func (c *Client) ImportVertex(ctx context.Context, request management.VertexImportRequest) (management.VertexImportResult, error) {
	file, err := os.Open(request.Path)
	if err != nil {
		return management.VertexImportResult{}, pmuxerr.Wrap(safeCause(err, "service-account file open failed"), pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read the Vertex service-account file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return management.VertexImportResult{}, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Vertex service-account input must be a regular file")
	}
	if info.Size() > c.maxResponse {
		return management.VertexImportResult{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Vertex service-account file exceeds the safety limit")
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreateFormFile("file", info.Name())
	if err != nil {
		return management.VertexImportResult{}, pmuxerr.Wrap(safeCause(err, "multipart creation failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not prepare the Vertex import")
	}
	if _, err = io.Copy(part, file); err != nil {
		return management.VertexImportResult{}, pmuxerr.Wrap(safeCause(err, "service-account read failed"), pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read the Vertex service-account file")
	}
	if request.Prefix != "" {
		_ = writer.WriteField("prefix", request.Prefix)
	}
	if err := writer.Close(); err != nil {
		return management.VertexImportResult{}, pmuxerr.Wrap(safeCause(err, "multipart close failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not prepare the Vertex import")
	}
	body, _, _, err := c.managementRequestType(ctx, http.MethodPost, "vertex/import", buffer.Bytes(), writer.FormDataContentType())
	if err != nil {
		return management.VertexImportResult{}, err
	}
	var result management.VertexImportResult
	if err := decodeJSON(body, &result); err != nil {
		return result, err
	}
	return result, nil
}
func (c *Client) ResetQuota(ctx context.Context, request management.ResetQuotaRequest) error {
	return c.jsonMutation(ctx, http.MethodPost, "reset-quota", request)
}
func (c *Client) LatestVersion(ctx context.Context) (string, error) {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, "latest-version", nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		Version       string `json:"version"`
		Latest        string `json:"latest"`
		LatestVersion string `json:"latest-version"`
	}
	if err := decodeJSON(body, &payload); err != nil {
		return "", err
	}
	if payload.Version != "" {
		return payload.Version, nil
	}
	if payload.Latest != "" {
		return payload.Latest, nil
	}
	return payload.LatestVersion, nil
}
func (c *Client) APICall(ctx context.Context, request management.APICallRequest) (management.APICallResponse, error) {
	payload := map[string]any{"method": request.Method, "url": request.URL, "headers": request.Headers, "body": request.Body}
	body, err := marshalBody(payload)
	if err != nil {
		return management.APICallResponse{}, err
	}
	responseBody, _, _, err := c.managementRequest(ctx, http.MethodPost, "api-call", body)
	if err != nil {
		return management.APICallResponse{}, err
	}
	var raw struct {
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    json.RawMessage     `json:"body"`
	}
	if err := decodeJSON(responseBody, &raw); err != nil {
		return management.APICallResponse{}, err
	}
	resultBody := redactResponseBody(c, raw.Body)
	for name, values := range raw.Headers {
		if redact.IsSensitiveKey(name) {
			raw.Headers[name] = []string{"********"}
			continue
		}
		for i := range values {
			values[i] = c.redact(values[i])
		}
	}
	return management.APICallResponse{Status: raw.Status, Headers: raw.Headers, Body: resultBody}, nil
}

func (c *Client) Plugins(ctx context.Context) (management.PluginList, error) {
	var value management.PluginList
	err := c.getJSON(ctx, "plugins", &value)
	return value, err
}
func (c *Client) PluginStore(ctx context.Context) (management.PluginStoreList, error) {
	var value management.PluginStoreList
	err := c.getJSON(ctx, "plugin-store", &value)
	return value, err
}
func (c *Client) InstallPlugin(ctx context.Context, id, source, version string) (management.PluginInstallResult, error) {
	query := make(url.Values)
	if source != "" {
		query.Set("source", source)
	}
	if version != "" {
		query.Set("version", version)
	}
	body, status, err := c.pluginMutation(ctx, http.MethodPost, []string{"plugin-store", id, "install"}, query, nil)
	if err != nil {
		return management.PluginInstallResult{}, err
	}
	if status == http.StatusConflict {
		return management.PluginInstallResult{}, c.pluginConflictError("install", id, body)
	}
	var result management.PluginInstallResult
	if err := decodeJSON(body, &result); err != nil {
		return management.PluginInstallResult{}, err
	}
	return result, nil
}
func (c *Client) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	body, err := marshalBody(map[string]bool{"enabled": enabled})
	if err != nil {
		return err
	}
	_, _, _, err = c.managementRequestParts(ctx, http.MethodPatch, []string{"plugins", id, "enabled"}, nil, body, "")
	return err
}
func (c *Client) PluginConfig(ctx context.Context, id string) (map[string]any, error) {
	body, _, _, err := c.managementRequestParts(ctx, http.MethodGet, []string{"plugins", id, "config"}, nil, nil, "")
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := decodeJSON(body, &config); err != nil {
		return nil, err
	}
	if config == nil {
		config = map[string]any{}
	}
	return config, nil
}
func (c *Client) PutPluginConfig(ctx context.Context, id string, config map[string]any) error {
	return c.pluginConfigMutation(ctx, http.MethodPut, id, config)
}
func (c *Client) PatchPluginConfig(ctx context.Context, id string, patch map[string]any) error {
	return c.pluginConfigMutation(ctx, http.MethodPatch, id, patch)
}
func (c *Client) pluginConfigMutation(ctx context.Context, method, id string, value map[string]any) error {
	if value == nil {
		value = map[string]any{}
	}
	body, err := marshalBody(value)
	if err != nil {
		return err
	}
	_, _, _, err = c.managementRequestParts(ctx, method, []string{"plugins", id, "config"}, nil, body, "")
	return err
}
func (c *Client) DeletePlugin(ctx context.Context, id string) (management.PluginDeleteResult, error) {
	body, status, err := c.pluginMutation(ctx, http.MethodDelete, []string{"plugins", id}, nil, nil)
	if err != nil {
		return management.PluginDeleteResult{}, err
	}
	if status == http.StatusConflict {
		return management.PluginDeleteResult{}, c.pluginConflictError("delete", id, body)
	}
	var result management.PluginDeleteResult
	if err := decodeJSON(body, &result); err != nil {
		return management.PluginDeleteResult{}, err
	}
	return result, nil
}

// pluginMutation performs a plugin mutation whose HTTP 409 response carries a
// structured body (for example restart_required) that the generic status
// classifier discards. It mirrors Client.request but returns the conflict
// payload so callers can fold the upstream detail into the typed error.
func (c *Client) pluginMutation(ctx context.Context, method string, parts []string, query url.Values, body []byte) ([]byte, int, error) {
	if c.managementKey == "" {
		return nil, 0, managementKeyMissing()
	}
	requestURL, err := url.Parse(c.managementEndpoint(parts...))
	if err != nil {
		return nil, 0, pmuxerr.Wrap(safeCause(err, "invalid request URL"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not construct the CLIProxyAPI request")
	}
	if len(query) != 0 {
		requestURL.RawQuery = query.Encode()
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, pmuxerr.Wrap(safeCause(err, "request creation failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not construct the CLIProxyAPI request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.managementKey)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(requestCtx.Err(), context.Canceled) {
			return nil, 0, pmuxerr.Wrap(context.Canceled, pmuxerr.CodeCanceled, pmuxerr.User, "The CLIProxyAPI request was canceled")
		}
		message := "Could not reach CLIProxyAPI"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			message = "CLIProxyAPI did not respond within " + c.timeout.String()
		}
		return nil, 0, pmuxerr.Wrap(safeCause(err, "HTTP transport failed"), pmuxerr.ManagementUnreachable, pmuxerr.Environment, message)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := readBounded(resp.Body, c.maxResponse)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return nil, resp.StatusCode, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, fmt.Sprintf("CLIProxyAPI response exceeded the %d-byte safety limit", c.maxResponse))
		}
		return nil, resp.StatusCode, pmuxerr.Wrap(safeCause(err, "response read failed"), pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Could not read the CLIProxyAPI response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusConflict {
		return payload, resp.StatusCode, nil
	}
	return nil, resp.StatusCode, c.statusError(ctx, requestSpec{management: true, classify404: true}, resp.StatusCode)
}

// pluginConflictError preserves the upstream 409 detail (for example
// plugin_update_requires_restart) that Client.request would discard.
func (c *Client) pluginConflictError(operation, id string, body []byte) error {
	var detail struct {
		Error           string `json:"error"`
		Status          string `json:"status"`
		Message         string `json:"message"`
		RestartRequired bool   `json:"restart_required"`
	}
	_ = json.Unmarshal(body, &detail)
	reason := detail.Error
	if reason == "" {
		reason = detail.Status
	}
	if reason == "" {
		reason = detail.Message
	}
	reason = c.redact(reason)
	message := fmt.Sprintf("CLIProxyAPI could not %s plugin %q (restart_required=%t)", operation, id, detail.RestartRequired)
	if reason != "" {
		message = fmt.Sprintf("CLIProxyAPI could not %s plugin %q: %s (restart_required=%t)", operation, id, reason, detail.RestartRequired)
	}
	return &pmuxerr.Error{
		Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Upstream, Message: message,
		Evidence: []string{fmt.Sprintf("http_status=%d", http.StatusConflict), fmt.Sprintf("restart_required=%t", detail.RestartRequired)},
	}
}

func redactResponseBody(c *Client, raw json.RawMessage) []byte {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []byte(c.redact(text))
	}
	var object map[string]any
	if json.Unmarshal(raw, &object) == nil {
		redactMap(c, object)
		encoded, err := json.Marshal(object)
		if err == nil {
			return encoded
		}
	}
	return []byte(c.redact(string(raw)))
}

func (c *Client) managementRequest(ctx context.Context, method, endpoint string, body []byte) ([]byte, http.Header, int, error) {
	return c.managementQuery(ctx, method, endpoint, nil, body)
}
func (c *Client) managementQuery(ctx context.Context, method, endpoint string, query url.Values, body []byte) ([]byte, http.Header, int, error) {
	return c.managementRequestTypeQuery(ctx, method, endpoint, query, body, "")
}
func (c *Client) managementRequestType(ctx context.Context, method, endpoint string, body []byte, contentType string) ([]byte, http.Header, int, error) {
	return c.managementRequestTypeQuery(ctx, method, endpoint, nil, body, contentType)
}
func (c *Client) managementRequestTypeQuery(ctx context.Context, method, endpoint string, query url.Values, body []byte, contentType string) ([]byte, http.Header, int, error) {
	parts := strings.Split(strings.Trim(endpoint, "/"), "/")
	return c.managementRequestParts(ctx, method, parts, query, body, contentType)
}
func (c *Client) managementRequestParts(ctx context.Context, method string, parts []string, query url.Values, body []byte, contentType string) ([]byte, http.Header, int, error) {
	return c.request(ctx, requestSpec{method: method, url: c.managementEndpoint(parts...), query: query, body: body, contentType: contentType, auth: authManagement, management: true, classify404: len(parts) != 1 || parts[0] != "config"})
}
func (c *Client) jsonMutation(ctx context.Context, method, endpoint string, value any) error {
	body, err := marshalBody(value)
	if err != nil {
		return err
	}
	_, _, _, err = c.managementRequest(ctx, method, endpoint, body)
	return err
}
func (c *Client) rawMutation(ctx context.Context, method, endpoint string, value []byte) error {
	if !json.Valid(value) {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Mutation payload must be valid JSON")
	}
	_, _, _, err := c.managementRequest(ctx, method, endpoint, value)
	return err
}
func (c *Client) getJSON(ctx context.Context, endpoint string, destination any) error {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return decodeJSON(body, destination)
}
func (c *Client) getListJSON(ctx context.Context, endpoint string, destination any, fields ...string) error {
	body, _, _, err := c.managementRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return err
	}
	items, err := rawArray(raw, fields...)
	if err != nil {
		return err
	}
	joined, err := json.Marshal(items)
	if err != nil {
		return pmuxerr.Wrap(safeCause(err, "list response encoding failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not normalize the CLIProxyAPI list response")
	}
	return decodeJSON(joined, destination)
}

func rawArray(raw json.RawMessage, fields ...string) ([]json.RawMessage, error) {
	var direct []json.RawMessage
	if json.Unmarshal(raw, &direct) == nil {
		return direct, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "CLIProxyAPI returned an unrecognized list response")
	}
	for _, field := range fields {
		if value, ok := object[field]; ok {
			if json.Unmarshal(value, &direct) == nil {
				return direct, nil
			}
		}
	}
	return nil, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "CLIProxyAPI returned an unrecognized list response")
}
func stringList(body []byte, fields ...string) ([]string, error) {
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return nil, err
	}
	items, err := rawArray(raw, fields...)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		var value string
		if json.Unmarshal(item, &value) == nil {
			values = append(values, value)
			continue
		}
		var object struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if json.Unmarshal(item, &object) == nil {
			if object.Key != "" {
				values = append(values, object.Key)
			} else {
				values = append(values, object.Value)
			}
			continue
		}
		return nil, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "CLIProxyAPI returned an unrecognized key response")
	}
	return values, nil
}
func decodeModelRefs(body []byte, c *Client) ([]management.ModelRef, error) {
	var raw json.RawMessage
	if err := decodeJSON(body, &raw); err != nil {
		return nil, err
	}
	items, err := rawArray(raw, "models", "data")
	if err != nil {
		return nil, err
	}
	values := make([]management.ModelRef, 0, len(items))
	for _, item := range items {
		var value management.ModelRef
		if err := decodeJSON(item, &value); err != nil {
			return nil, err
		}
		value.Owner = c.redact(value.Owner)
		value.Channel = c.redact(value.Channel)
		values = append(values, value)
	}
	return values, nil
}
func redactMap(c *Client, object map[string]any) {
	for key, value := range object {
		switch typed := value.(type) {
		case string:
			if isSensitiveField(key) {
				object[key] = redact.Mask(typed)
			} else {
				object[key] = c.redact(typed)
			}
		case map[string]any:
			redactMap(c, typed)
		case []any:
			for i, item := range typed {
				if text, ok := item.(string); ok && isSensitiveField(key) {
					typed[i] = redact.Mask(text)
				} else if child, ok := item.(map[string]any); ok {
					redactMap(c, child)
				}
			}
		}
	}
}
func redactStringMap(c *Client, object map[string]string) {
	for key, value := range object {
		if isSensitiveField(key) {
			object[key] = redact.Mask(value)
		} else {
			object[key] = c.redact(value)
		}
	}
}
func isSensitiveField(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return redact.IsSensitiveKey(normalized) || normalized == "api-keys" ||
		strings.HasSuffix(normalized, "-keys") || strings.HasSuffix(normalized, "_keys")
}

var _ management.ManagementClient = (*Client)(nil)
