package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

// aliasChannels are the CLIProxyAPI oauth-model-alias and oauth-excluded-models
// channel names. Values are validated against this set so typos never persist
// into the core configuration.
var aliasChannels = []string{"aistudio", "antigravity", "claude", "codex", "kimi", "vertex", "xai"}

func validChannel(raw string) (string, error) {
	channel := strings.ToLower(strings.TrimSpace(raw))
	for _, candidate := range aliasChannels {
		if channel == candidate {
			return channel, nil
		}
	}
	return "", typedUsage(fmt.Sprintf("Unknown channel %q; supported channels are %s.", raw, strings.Join(aliasChannels, ", ")))
}

// requireMutationConsent enforces the shared mutation contract: noninteractive
// callers pass --yes, interactive callers confirm explicitly.
func (r *Router) requireMutationConsent(in Invocation, action string) error {
	if !in.Interactive && !in.Yes {
		return typedUsage(fmt.Sprintf("Noninteractive %s requires `--yes`; no changes were made.", action))
	}
	if in.Interactive && !in.Yes {
		ok, err := r.confirmPhrase("write")
		if err != nil {
			return err
		}
		if !ok {
			return canceled(fmt.Errorf("%s was not confirmed", action))
		}
	}
	return nil
}

// --- Client API keys -------------------------------------------------------

func (r *Router) keys(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("manage client API keys", "Run `pmux setup --mode managed` first.")
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	switch in.Operation {
	case OpKeysList:
		keys, err := client.APIKeys(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not list client API keys.")
		}
		usage, usageErr := client.APIKeyUsage(ctx)
		requests := map[string]int64{}
		if usageErr == nil {
			for _, record := range usage {
				requests[record.Fingerprint] = record.Requests
			}
		}
		rows := make([]map[string]any, 0, len(keys))
		human := make([]string, 0, len(keys)+1)
		for _, key := range keys {
			rows = append(rows, map[string]any{"mask": key.Mask, "fingerprint": key.Fingerprint, "requests": requests[key.Fingerprint]})
			human = append(human, fmt.Sprintf("%s  fingerprint=%s  requests=%d", key.Mask, key.Fingerprint, requests[key.Fingerprint]))
		}
		if len(human) == 0 {
			human = append(human, "No client API keys are configured.")
		}
		return Result{Data: map[string]any{"keys": rows, "usage_available": usageErr == nil}, Human: human}, nil
	case OpKeysAdd:
		if err := r.requireMutationConsent(in, "client API key creation"); err != nil {
			return Result{}, err
		}
		var secret []byte
		switch {
		case optionBool(in, "generate"):
			secret = make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return Result{}, normalize(err, pmuxerr.UnhandledInternal, "PMux could not generate a random API key.")
			}
			secret = []byte(hex.EncodeToString(secret))
		case optionString(in, "api_key_file") != "":
			input := &fileProtectedInput{path: optionString(in, "api_key_file"), verify: r.deps.VerifyPrivateFile}
			value, err := input.ReadSecret(ctx)
			if err != nil {
				return Result{}, ensureTyped(err, "PMux could not read the API key file.")
			}
			secret = value
		case optionBool(in, "api_key_stdin"):
			input := &readerProtectedInput{reader: r.deps.Input}
			value, err := input.ReadSecret(ctx)
			if err != nil {
				return Result{}, ensureTyped(err, "PMux could not read the API key from standard input.")
			}
			secret = value
		default:
			if !in.Interactive {
				return Result{}, typedUsage("Noninteractive key creation requires --generate, --api-key-file, or --api-key-stdin.")
			}
			if r.deps.ReadPassword == nil {
				return Result{}, dependencyError("Protected terminal input is unavailable.", "Use --generate, --api-key-file, or --api-key-stdin.")
			}
			input := &promptProtectedInput{read: r.deps.ReadPassword, prompt: "Enter the new client API key: "}
			value, err := input.ReadSecret(ctx)
			if err != nil {
				return Result{}, ensureTyped(err, "PMux could not read the API key.")
			}
			secret = value
		}
		defer clearBytes(secret)
		trimmed := strings.TrimSpace(string(secret))
		if trimmed == "" {
			return Result{}, typedUsage("The client API key must not be empty.")
		}
		existing, err := client.APIKeys(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read the existing client API keys.")
		}
		_ = existing
		if err := r.emitPreview(sink, installation.ID, "Preview: add one client API key through the Management API.", map[string]any{"operation": "keys.add"}); err != nil {
			return Result{}, err
		}
		patch, err := json.Marshal(map[string]any{"add": []string{trimmed}})
		if err != nil {
			return Result{}, normalize(err, pmuxerr.UnhandledInternal, "PMux could not encode the API key patch.")
		}
		if err := client.PatchAPIKeys(ctx, management.KeyPatch(patch)); err != nil {
			return Result{}, ensureTyped(err, "PMux could not add the client API key.")
		}
		data := map[string]any{"added": true}
		human := []string{"Client API key added."}
		if optionBool(in, "generate") {
			// The generated key exists nowhere else yet; it is returned once so
			// the caller can store it. Stored keys are never printed again.
			data["api_key"] = trimmed
			human = append(human, "Generated key (shown once): "+trimmed)
		}
		return Result{Data: data, Human: human}, nil
	case OpKeysRemove:
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("keys remove requires one key fingerprint from `pmux keys list`.")
		}
		if err := r.requireMutationConsent(in, "client API key removal"); err != nil {
			return Result{}, err
		}
		if err := r.emitPreview(sink, installation.ID, "Preview: remove one client API key through the Management API.", map[string]any{"operation": "keys.remove", "fingerprint": in.Arguments[0]}); err != nil {
			return Result{}, err
		}
		if err := client.DeleteAPIKey(ctx, in.Arguments[0]); err != nil {
			return Result{}, ensureTyped(err, "PMux could not remove the client API key.")
		}
		return Result{Data: map[string]any{"removed": true, "fingerprint": in.Arguments[0]}, Human: []string{"Client API key removed."}}, nil
	default:
		return Result{}, typedUsage(fmt.Sprintf("Unsupported keys operation %q.", in.Operation))
	}
}

// --- Model aliases and exclusions ------------------------------------------

func (r *Router) modelsAliases(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("manage model aliases", "Run `pmux setup --mode managed` first.")
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	action := valueOr(optionString(in, "action"), "list")
	if action == "list" {
		aliases, err := client.ModelAliases(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not list model aliases.")
		}
		human := make([]string, 0)
		channels := make([]string, 0, len(aliases))
		for channel := range aliases {
			channels = append(channels, channel)
		}
		sort.Strings(channels)
		for _, channel := range channels {
			names := make([]string, 0, len(aliases[channel]))
			for alias := range aliases[channel] {
				names = append(names, alias)
			}
			sort.Strings(names)
			for _, alias := range names {
				human = append(human, fmt.Sprintf("%s: %s -> %s", channel, alias, aliases[channel][alias]))
			}
		}
		if len(human) == 0 {
			human = append(human, "No model aliases are configured.")
		}
		return Result{Data: map[string]any{"aliases": aliases}, Human: human}, nil
	}
	if err := r.requireMutationConsent(in, "model alias change"); err != nil {
		return Result{}, err
	}
	switch action {
	case "set":
		if len(in.Arguments) != 3 {
			return Result{}, typedUsage("aliases set requires <channel> <alias> <exact-model-id>.")
		}
		channel, err := validChannel(in.Arguments[0])
		if err != nil {
			return Result{}, err
		}
		alias, model := in.Arguments[1], in.Arguments[2]
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(model) == "" {
			return Result{}, typedUsage("Alias and model ID must not be empty.")
		}
		aliases, err := client.ModelAliases(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read model aliases.")
		}
		if aliases == nil {
			aliases = management.ModelAliasSet{}
		}
		if aliases[channel] == nil {
			aliases[channel] = map[string]string{}
		}
		aliases[channel][alias] = model
		if err := r.emitPreview(sink, installation.ID, fmt.Sprintf("Preview: alias %s -> %s on channel %s.", alias, model, channel), map[string]any{"channel": channel, "alias": alias, "model": model}); err != nil {
			return Result{}, err
		}
		if err := client.PutModelAliases(ctx, aliases); err != nil {
			return Result{}, ensureTyped(err, "PMux could not save model aliases.")
		}
		return Result{Data: map[string]any{"channel": channel, "alias": alias, "model": model}, Human: []string{fmt.Sprintf("Aliased %s to %s on channel %s.", alias, model, channel)}}, nil
	case "remove":
		if len(in.Arguments) != 2 {
			return Result{}, typedUsage("aliases remove requires <channel> <alias>.")
		}
		channel, err := validChannel(in.Arguments[0])
		if err != nil {
			return Result{}, err
		}
		aliases, err := client.ModelAliases(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read model aliases.")
		}
		if _, ok := aliases[channel][in.Arguments[1]]; !ok {
			return Result{}, typedUsage(fmt.Sprintf("Alias %q is not configured on channel %s.", in.Arguments[1], channel))
		}
		delete(aliases[channel], in.Arguments[1])
		if len(aliases[channel]) == 0 {
			delete(aliases, channel)
		}
		if err := client.PutModelAliases(ctx, aliases); err != nil {
			return Result{}, ensureTyped(err, "PMux could not save model aliases.")
		}
		return Result{Data: map[string]any{"channel": channel, "removed": in.Arguments[1]}, Human: []string{fmt.Sprintf("Removed alias %s on channel %s.", in.Arguments[1], channel)}}, nil
	case "clear":
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("aliases clear requires <channel>.")
		}
		channel, err := validChannel(in.Arguments[0])
		if err != nil {
			return Result{}, err
		}
		if err := client.DeleteModelAliases(ctx, channel); err != nil {
			return Result{}, ensureTyped(err, "PMux could not clear model aliases.")
		}
		return Result{Data: map[string]any{"channel": channel, "cleared": true}, Human: []string{fmt.Sprintf("Cleared all aliases on channel %s.", channel)}}, nil
	default:
		return Result{}, typedUsage(fmt.Sprintf("Unknown aliases action %q; use list, set, remove, or clear.", action))
	}
}

func (r *Router) modelsExclusions(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("manage model exclusions", "Run `pmux setup --mode managed` first.")
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	action := valueOr(optionString(in, "action"), "list")
	if action == "list" {
		excluded, err := client.ExcludedModels(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not list excluded models.")
		}
		human := make([]string, 0)
		channels := make([]string, 0, len(excluded))
		for channel := range excluded {
			channels = append(channels, channel)
		}
		sort.Strings(channels)
		for _, channel := range channels {
			for _, pattern := range excluded[channel] {
				human = append(human, fmt.Sprintf("%s: %s", channel, pattern))
			}
		}
		if len(human) == 0 {
			human = append(human, "No model exclusions are configured.")
		}
		return Result{Data: map[string]any{"exclusions": excluded}, Human: human}, nil
	}
	if err := r.requireMutationConsent(in, "model exclusion change"); err != nil {
		return Result{}, err
	}
	switch action {
	case "add", "remove":
		if len(in.Arguments) != 2 {
			return Result{}, typedUsage("exclusions " + action + " requires <channel> <pattern>.")
		}
		channel, err := validChannel(in.Arguments[0])
		if err != nil {
			return Result{}, err
		}
		pattern := in.Arguments[1]
		if strings.TrimSpace(pattern) == "" {
			return Result{}, typedUsage("Exclusion pattern must not be empty.")
		}
		excluded, err := client.ExcludedModels(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read excluded models.")
		}
		if excluded == nil {
			excluded = management.ExcludedModelSet{}
		}
		patterns := excluded[channel]
		if action == "add" {
			for _, existing := range patterns {
				if existing == pattern {
					return Result{Data: map[string]any{"channel": channel, "pattern": pattern, "changed": false}, Human: []string{"Pattern is already excluded."}}, nil
				}
			}
			excluded[channel] = append(patterns, pattern)
		} else {
			kept := patterns[:0]
			found := false
			for _, existing := range patterns {
				if existing == pattern {
					found = true
					continue
				}
				kept = append(kept, existing)
			}
			if !found {
				return Result{}, typedUsage(fmt.Sprintf("Pattern %q is not excluded on channel %s.", pattern, channel))
			}
			if len(kept) == 0 {
				delete(excluded, channel)
			} else {
				excluded[channel] = kept
			}
		}
		if err := r.emitPreview(sink, installation.ID, fmt.Sprintf("Preview: %s exclusion %s on channel %s.", action, pattern, channel), map[string]any{"channel": channel, "pattern": pattern, "action": action}); err != nil {
			return Result{}, err
		}
		if err := client.PutExcludedModels(ctx, excluded); err != nil {
			return Result{}, ensureTyped(err, "PMux could not save excluded models.")
		}
		return Result{Data: map[string]any{"channel": channel, "pattern": pattern, "changed": true}, Human: []string{fmt.Sprintf("Updated exclusions on channel %s.", channel)}}, nil
	case "clear":
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("exclusions clear requires <channel>.")
		}
		channel, err := validChannel(in.Arguments[0])
		if err != nil {
			return Result{}, err
		}
		if err := client.DeleteExcludedModels(ctx, channel); err != nil {
			return Result{}, ensureTyped(err, "PMux could not clear excluded models.")
		}
		return Result{Data: map[string]any{"channel": channel, "cleared": true}, Human: []string{fmt.Sprintf("Cleared exclusions on channel %s.", channel)}}, nil
	default:
		return Result{}, typedUsage(fmt.Sprintf("Unknown exclusions action %q; use list, add, remove, or clear.", action))
	}
}

// --- Quota ------------------------------------------------------------------

func (r *Router) providerResetQuota(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("reset provider quota", "Run `pmux setup --mode managed` first.")
	}
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("reset-quota requires one auth file name from `pmux providers list`.")
	}
	if err := r.requireMutationConsent(in, "quota reset"); err != nil {
		return Result{}, err
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	if err := client.ResetQuota(ctx, management.ResetQuotaRequest{Name: in.Arguments[0]}); err != nil {
		return Result{}, ensureTyped(err, "PMux could not reset the quota state.")
	}
	return Result{Data: map[string]any{"auth_file": in.Arguments[0], "reset": true}, Human: []string{fmt.Sprintf("Reset quota state for %s.", in.Arguments[0])}}, nil
}

// --- Plugins -----------------------------------------------------------------

func (r *Router) plugins(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("manage plugins", "Run `pmux setup --mode managed` first.")
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	switch in.Operation {
	case OpPluginsList:
		list, err := client.Plugins(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not list plugins.")
		}
		human := make([]string, 0, len(list.Plugins)+1)
		for _, plugin := range list.Plugins {
			state := "disabled"
			if plugin.EffectiveEnabled {
				state = "enabled"
			} else if plugin.Enabled {
				state = "enabled (inactive)"
			}
			human = append(human, fmt.Sprintf("%s  %s  %s", plugin.ID, plugin.Metadata.Version, state))
		}
		if len(human) == 0 {
			human = append(human, "No plugins are installed.")
		}
		return Result{Data: list, Human: append([]string{fmt.Sprintf("plugins_enabled=%v dir=%s", list.PluginsEnabled, list.PluginsDir)}, human...)}, nil
	case OpPluginStore:
		store, err := client.PluginStore(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not list the plugin store.")
		}
		human := make([]string, 0, len(store.Plugins)+1)
		for _, plugin := range store.Plugins {
			installed := ""
			if plugin.Installed {
				installed = " (installed " + plugin.InstalledVersion + ")"
			}
			human = append(human, fmt.Sprintf("%s  %s  %s%s", plugin.StoreID, plugin.Version, plugin.Description, installed))
		}
		if len(human) == 0 {
			human = append(human, "No store plugins are available.")
		}
		return Result{Data: store, Human: human}, nil
	case OpPluginInstall:
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("plugins install requires one store plugin ID.")
		}
		if err := r.requireMutationConsent(in, "plugin install"); err != nil {
			return Result{}, err
		}
		if err := r.emitPreview(sink, installation.ID, fmt.Sprintf("Preview: install plugin %s from the store.", in.Arguments[0]), map[string]any{"plugin": in.Arguments[0]}); err != nil {
			return Result{}, err
		}
		result, err := client.InstallPlugin(ctx, in.Arguments[0], optionString(in, "source"), optionString(in, "version"))
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not install the plugin.")
		}
		human := []string{fmt.Sprintf("Installed plugin %s %s.", result.ID, result.Version)}
		if result.RestartRequired {
			human = append(human, "A service restart is required before the plugin is active.")
		}
		return Result{Data: result, Human: human}, nil
	case OpPluginSetEnabled:
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("plugins enable/disable requires one plugin ID.")
		}
		if err := r.requireMutationConsent(in, "plugin state change"); err != nil {
			return Result{}, err
		}
		enabled := optionBool(in, "enabled")
		if err := client.SetPluginEnabled(ctx, in.Arguments[0], enabled); err != nil {
			return Result{}, ensureTyped(err, "PMux could not change the plugin state.")
		}
		word := "disabled"
		if enabled {
			word = "enabled"
		}
		return Result{Data: map[string]any{"plugin": in.Arguments[0], "enabled": enabled}, Human: []string{fmt.Sprintf("Plugin %s %s.", in.Arguments[0], word)}}, nil
	case OpPluginConfigShow:
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("plugins config show requires one plugin ID.")
		}
		config, err := client.PluginConfig(ctx, in.Arguments[0])
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read the plugin configuration.")
		}
		return Result{Data: map[string]any{"plugin": in.Arguments[0], "config": config}, Human: []string{formatJSON(config)}}, nil
	case OpPluginConfigSet:
		if len(in.Arguments) != 2 {
			return Result{}, typedUsage("plugins config set requires <plugin> <json-object>.")
		}
		if err := r.requireMutationConsent(in, "plugin configuration change"); err != nil {
			return Result{}, err
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(in.Arguments[1]), &body); err != nil {
			return Result{}, typedUsage("Plugin configuration must be a JSON object.")
		}
		if optionBool(in, "patch") {
			if err := client.PatchPluginConfig(ctx, in.Arguments[0], body); err != nil {
				return Result{}, ensureTyped(err, "PMux could not patch the plugin configuration.")
			}
		} else if err := client.PutPluginConfig(ctx, in.Arguments[0], body); err != nil {
			return Result{}, ensureTyped(err, "PMux could not save the plugin configuration.")
		}
		return Result{Data: map[string]any{"plugin": in.Arguments[0], "patched": optionBool(in, "patch")}, Human: []string{fmt.Sprintf("Saved configuration for plugin %s.", in.Arguments[0])}}, nil
	case OpPluginRemove:
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("plugins remove requires one plugin ID.")
		}
		if err := r.requireMutationConsent(in, "plugin removal"); err != nil {
			return Result{}, err
		}
		result, err := client.DeletePlugin(ctx, in.Arguments[0])
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not remove the plugin.")
		}
		human := []string{fmt.Sprintf("Removed plugin %s.", in.Arguments[0])}
		if result.RestartRequired {
			human = append(human, "A service restart is required to finish unloading the plugin.")
		}
		return Result{Data: result, Human: human}, nil
	default:
		return Result{}, typedUsage(fmt.Sprintf("Unsupported plugins operation %q.", in.Operation))
	}
}

func formatJSON(value any) string {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(body)
}

// --- Management panel ---------------------------------------------------------

func (r *Router) panel(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("open the management panel", "Run `pmux setup --mode managed` first.")
	}
	url := baseURL(installation) + "/management.html"
	data := map[string]any{"url": url, "opened": false}
	human := []string{"Management panel: " + url}
	if optionBool(in, "open") {
		if r.deps.OpenURL == nil {
			return Result{}, dependencyError("Browser handoff is unavailable.", "Open the printed URL manually.")
		}
		if err := r.deps.OpenURL(ctx, url); err != nil {
			return Result{}, ensureTyped(err, "PMux could not open the management panel in a browser.")
		}
		data["opened"] = true
		human = append(human, "Opened in the default browser.")
	}
	return Result{Data: data, Human: human}, nil
}

// --- Profiles ------------------------------------------------------------------

func validateProfileClient(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "claude", "codex", "gemini", "opencode":
		return strings.ToLower(strings.TrimSpace(raw)), nil
	default:
		return "", typedUsage(fmt.Sprintf("Unsupported profile client %q; use claude, codex, gemini, or opencode.", raw))
	}
}

func (r *Router) profilesList(in Invocation, cfg state.Config) (Result, error) {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	if in.Operation == OpProfilesShow {
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("profiles show requires one profile name.")
		}
		profile, ok := cfg.Profiles[in.Arguments[0]]
		if !ok {
			return Result{}, typedUsage(fmt.Sprintf("Profile %q is not defined. Run `pmux profiles list`.", in.Arguments[0]))
		}
		return Result{Data: map[string]any{"name": in.Arguments[0], "profile": profile}, Human: []string{formatJSON(profile)}}, nil
	}
	human := make([]string, 0, len(names)+1)
	rows := make([]map[string]any, 0, len(names))
	for _, name := range names {
		profile := cfg.Profiles[name]
		rows = append(rows, map[string]any{"name": name, "client": profile.Client, "model": profile.Model, "fallback": profile.Fallback})
		line := fmt.Sprintf("%s  client=%s model=%s", name, profile.Client, profile.Model)
		if len(profile.Fallback) > 0 {
			line += " fallback=" + strings.Join(profile.Fallback, ",")
		}
		human = append(human, line)
	}
	if len(human) == 0 {
		human = append(human, "No profiles are defined. Create one with `pmux profiles set <name> --client <client> --model <id>`.")
	}
	return Result{Data: map[string]any{"profiles": rows}, Human: human}, nil
}

func (r *Router) profileSet(in Invocation, cfg state.Config) (Result, error) {
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("profiles set requires one profile name.")
	}
	name := strings.TrimSpace(in.Arguments[0])
	if name == "" || strings.ContainsAny(name, " \t\n/\\") {
		return Result{}, typedUsage("Profile names must be non-empty and contain no whitespace or path separators.")
	}
	client, err := validateProfileClient(optionString(in, "client"))
	if err != nil {
		return Result{}, err
	}
	model := strings.TrimSpace(optionString(in, "model"))
	if model == "" {
		return Result{}, typedUsage("profiles set requires --model with an exact dynamic model ID.")
	}
	fallback := optionStrings(in, "fallback")
	for _, candidate := range append([]string{model}, fallback...) {
		if strings.TrimSpace(candidate) == "" {
			return Result{}, typedUsage("Profile model IDs must not be empty.")
		}
	}
	args := optionStrings(in, "args")
	if hasModelFlag(args) {
		return Result{}, typedUsage("Profile client arguments must not set the model; use --model and --fallback.")
	}
	if err := r.requireMutationConsent(in, "profile change"); err != nil {
		return Result{}, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]state.Profile{}
	}
	cfg.Profiles[name] = state.Profile{Client: client, Model: model, Fallback: fallback, Args: args}
	if err := r.deps.Store.SaveConfig(cfg); err != nil {
		return Result{}, normalize(err, pmuxerr.ConfigMutationConflict, "PMux settings could not be committed.")
	}
	return Result{Data: map[string]any{"name": name, "profile": cfg.Profiles[name]}, Human: []string{fmt.Sprintf("Saved profile %s (client=%s model=%s).", name, client, model)}}, nil
}

func (r *Router) profileRemove(in Invocation, cfg state.Config) (Result, error) {
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("profiles remove requires one profile name.")
	}
	if _, ok := cfg.Profiles[in.Arguments[0]]; !ok {
		return Result{}, typedUsage(fmt.Sprintf("Profile %q is not defined.", in.Arguments[0]))
	}
	if err := r.requireMutationConsent(in, "profile removal"); err != nil {
		return Result{}, err
	}
	delete(cfg.Profiles, in.Arguments[0])
	if err := r.deps.Store.SaveConfig(cfg); err != nil {
		return Result{}, normalize(err, pmuxerr.ConfigMutationConflict, "PMux settings could not be committed.")
	}
	return Result{Data: map[string]any{"removed": in.Arguments[0]}, Human: []string{fmt.Sprintf("Removed profile %s.", in.Arguments[0])}}, nil
}

func hasModelFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model=") {
			return true
		}
	}
	return false
}
