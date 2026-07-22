# Coding client integration

PMux launches coding clients against the selected CLIProxyAPI instance with process-scoped credentials. Supported clients:

| Client | Alias | Minimum detection | Transport |
|---|---|---|---|
| Claude Code | `pmux claude <id>` | `claude --version`, semver 2.0.0+ | Anthropic `/v1/messages` |
| Codex CLI | `pmux codex <id>` | `codex --version` (`codex-cli X.Y.Z`) | OpenAI `/v1/responses` |
| Gemini CLI | `pmux gemini <id>` | `gemini --version` | Gemini `/v1beta/*` |
| OpenCode | `pmux opencode <id>` | `opencode --version` | OpenAI `/v1/chat/completions` |

Missing or unparseable client executables are blocked before spawn. PMux does not install or upgrade clients, and does not expose unavailable clients as no-op choices. Factory Droid is later.

## Launch

First obtain an exact current model ID:

```sh
pmux models list --refresh
```

Then launch:

```sh
pmux launch --client claude --model <exact-discovered-id>
pmux launch --client codex --model <exact-discovered-id> -- --ask-for-approval never
pmux launch --client gemini --model <exact-discovered-id>
pmux launch --client opencode --model <exact-discovered-id>
```

The aliases are token-for-token equivalent:

```sh
pmux claude <exact-discovered-id>
pmux codex <exact-discovered-id>
pmux gemini <exact-discovered-id>
pmux opencode <exact-discovered-id>
```

In the TUI, the Launch screen's `c` key cycles the target client (claude → codex → gemini → opencode); the Models screen's `l` launches the selected model with the same chosen client.

Arguments after `--` remain separate argv values; PMux does not invoke a shell. A passthrough `--model` (or `-m`) is rejected because the canonical PMux model option owns model selection. Model IDs are case-sensitive and pass unchanged, including punctuation or reasoning suffixes returned by the core.

## Fallback models and profiles

`--fallback` lists exact model IDs tried in order when the primary model is not currently served:

```sh
pmux launch --client claude --model <primary-id> --fallback <second-id>,<third-id>
```

Named profiles store a client, model, fallback chain, and extra client arguments in PMux configuration:

```sh
pmux profiles set work --client codex --model <primary-id> --fallback <second-id> --arg --quiet
pmux profiles list
pmux profiles show work
pmux launch --profile work
pmux profiles remove work
```

Explicit `--client`, `--model`, `--fallback`, and passthrough arguments override the matching profile fields. Fallback is a launch-time selection among live catalog entries; provider-level retry and cooldown remain CLIProxyAPI's job (`routing/strategy`, `request-retry`).

## Preflight

PMux checks, in order:

1. selected installation, absolute config path, loopback base URL, and private key source;
2. `GET /healthz` within two seconds;
3. proxy authentication and absence of safe mode;
4. exact model presence in a fresh/live catalog, walking the fallback chain when configured;
5. client executable detection and version parse;
6. working-directory existence and accessibility;
7. passthrough argument safety.

Failure blocks launch and gives one canonical next action such as `pmux service start`, `pmux models list --refresh`, or `pmux doctor`. PMux never substitutes a favorite, recent model, provider default, or static fallback beyond the explicit fallback chain.

## Exact child contracts

Credentials are process-scoped: they travel in the child environment or flags, never in PMux logs, JSON output, the parent shell, or the user's own client configuration files.

### Claude Code

PMux removes conflicting inherited Anthropic routing/auth names and adds only:

```text
ANTHROPIC_BASE_URL=http://127.0.0.1:<configured-port>
ANTHROPIC_AUTH_TOKEN=<proxy key read transiently from its private source>
```

It executes `claude --model <exact-id> [client argv...]`. Ordinary launch does not set `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`, or any default opus/sonnet/haiku slot.

### Codex CLI

PMux strips inherited `OPENAI_API_KEY`, `CODEX_API_KEY`, `OPENAI_BASE_URL`, `CODEX_HOME`, and `CODEX_MODEL`, sets `OPENAI_API_KEY` to the proxy key, and runs:

```text
codex -m <exact-id> \
  -c 'model_providers.pmux={ name = "pmux", base_url = "http://127.0.0.1:<port>/v1", env_key = "OPENAI_API_KEY", wire_api = "responses" }' \
  -c model_provider="pmux" [client argv...]
```

The custom `pmux` provider leaves `requires_openai_auth` false, so no login flow or `~/.codex/config.toml` edit occurs. Passthrough `-c`/`--config` is rejected for the same reason as `--model`: PMux owns the provider definition.

### Gemini CLI

PMux strips inherited `GEMINI_*`/`GOOGLE_*` routing variables and sets:

```text
GEMINI_API_KEY=<proxy key>
GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:<configured-port>
GEMINI_API_KEY_AUTH_MECHANISM=bearer
GEMINI_MODEL=<exact-id>
GEMINI_CLI_HOME=<pmux-owned isolated settings directory>
GEMINI_CLI_TRUST_WORKSPACE=true
GEMINI_TELEMETRY_ENABLED=false
```

It executes `gemini --skip-trust -m <exact-id> [client argv...]`. The isolated `GEMINI_CLI_HOME` holds only PMux-written settings (`selectedType: gemini-api-key`, telemetry and usage statistics off); the user's real `~/.gemini` is never read or written.

### OpenCode

PMux strips inherited `OPENCODE_*` configuration variables and sets `OPENCODE_CONFIG_CONTENT` to a process-scoped JSON document defining one `pmux` provider (`@ai-sdk/openai-compatible`, base URL `http://127.0.0.1:<port>/v1`, proxy key, exact model) plus `OPENCODE_DISABLE_PROJECT_CONFIG=1`. It executes `opencode [client argv...]`; the model comes from the injected config, never argv. No user `opencode.json` or `auth.json` is touched.

## Persistent Claude model slots

Persistence is separate from setup, launch, favorites, and recent selections, and is available only for Claude Code. Each slot requires its own exact live model ID:

```sh
pmux config --scope pmux set integrations.claude.persistent-models.opus <exact-id>
pmux config --scope pmux set integrations.claude.persistent-models.sonnet <exact-id>
pmux config --scope pmux set integrations.claude.persistent-models.haiku <exact-id>
```

Use the explicit value `unmanaged` on one of the same keys to stop PMux management for only that slot.

Before a persistent write PMux fingerprints and privately backs up the Claude settings file, previews a redacted diff, requires confirmation or global `--yes`, merges only chosen environment keys, writes atomically, re-parses, and verifies. It never copies one model into multiple slots. Connection variables are persisted only under the same explicit transaction; complete token values remain masked in every preview.

Removal changes only PMux-recorded values when the current fingerprint matches. A concurrent or ambiguous user change is an ownership conflict, not permission to restore over user work.

Process-scoped launch remains the default regardless of persistent settings.

## Troubleshooting

```sh
pmux doctor --check CLI-CLAUDE
pmux models list --refresh
pmux service status
```

If a client is absent, install it with its owning distribution method. If a model disappeared, select an ID from the new live catalog rather than editing a cached preference.
