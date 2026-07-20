# Claude Code client integration

Claude Code is the only v1 coding client. PMux requires a resolved executable whose `claude --version` output parses as semantic version 2.0.0 or newer. Missing, older, or unparseable versions are blocked before spawn.

Codex CLI, Gemini CLI, OpenCode, named profiles, and fallback chains are phase 2. Factory Droid is later. PMux does not expose unavailable clients as no-op choices.

## Launch

First obtain an exact current model ID:

```sh
pmux models list --refresh
```

Then launch:

```sh
pmux launch --client claude --model <exact-discovered-id>
pmux launch --client claude --model <exact-discovered-id> -- --permission-mode plan
```

The only alias is token-for-token equivalent:

```sh
pmux claude <exact-discovered-id> -- --permission-mode plan
```

Arguments after `--` remain separate argv values; PMux does not invoke a shell. A passthrough `--model` is rejected because the canonical PMux model option owns model selection. Model IDs are case-sensitive and pass unchanged, including punctuation or reasoning suffixes returned by the core.

## Preflight

PMux checks, in order:

1. selected installation, absolute config path, loopback base URL, and private key source;
2. `GET /healthz` within two seconds;
3. proxy authentication and absence of safe mode;
4. exact model presence in a fresh/live catalog;
5. Claude executable and version at least 2.0.0;
6. working-directory existence and accessibility;
7. passthrough argument safety.

Failure blocks launch and gives one canonical next action such as `pmux service start`, `pmux models list --refresh`, or `pmux doctor`. PMux never substitutes a favorite, recent model, provider default, or static fallback.

## Exact child contract

PMux removes conflicting inherited Anthropic routing/auth names and adds only:

```text
ANTHROPIC_BASE_URL=http://127.0.0.1:<configured-port>
ANTHROPIC_AUTH_TOKEN=<proxy key read transiently from its private source>
```

It executes:

```text
claude --model <exact-discovered-id> [client argv...]
```

Ordinary launch does not set `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`, or any default opus/sonnet/haiku slot. The token is never rendered, returned in JSON, copied, logged, or written into the parent shell. Unrelated parent environment variables remain inherited, while the parent process environment and Claude settings file remain unchanged.

On Unix/WSL PMux restores terminal state and replaces its process image with Claude. On Windows it starts a child with inherited stdio, forwards console interruption, waits, and returns the child status. Child statuses after handoff are client-origin statuses; pre-start errors use PMux's stable exit model.

Attached launch is an interactive stream rather than a finite machine-readable operation; use `pmux --json`, model listing, and doctor for automation/preflight data.

## Persistent Claude model slots

Persistence is separate from setup, launch, favorites, and recent selections. Each slot requires its own exact live model ID:

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

If the client is absent, install Claude Code v2.0.0 or newer with its owning distribution method. PMux does not install or upgrade Claude Code. If a model disappeared, select an ID from the new live catalog rather than editing a cached preference.
