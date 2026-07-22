# Providers and authentication

PMux orchestrates CLIProxyAPI authentication; it does not implement OAuth, token refresh, credential routing, or provider protocols. The integration order is Management API, structured configuration when needed, then a closed set of source-verified CLIProxyAPI subprocess flags. PMux never derives an upstream flag from a provider name.

## v1 provider contract

| Provider/class | Methods |
|---|---|
| Codex | callback OAuth and device authorization |
| Claude | callback OAuth |
| Antigravity | callback OAuth |
| Kimi | device authorization |
| xAI | runtime-detected callback or device response; xAI API key is a separate class |
| Gemini | protected API-key configuration |
| Vertex | service-account import with optional prefix; API-key configuration |
| OpenRouter/custom OpenAI-compatible | display name, base URL, protected API-key entries |
| Claude-compatible | base URL/config fields and protected API key |
| Codex-compatible | required base URL and protected API key |
| Interactions API | protected API-key configuration |

The xAI source and documentation have differed about callback versus device behavior. PMux renders only the shape returned by the supported running core and fails closed on an unrecognized payload. Release acceptance must exercise the actual v7.2.91 and v7.2.92 behavior; documentation does not guess it.

## Browse and verify

```sh
pmux providers list --refresh
pmux providers verify
pmux providers verify codex --refresh-models
```

Bare `pmux providers` opens the Providers TUI with a TTY and otherwise behaves as `pmux providers list`. `providers list` returns stable provider IDs, capability classes, enabled/authentication state, usable/total accounts, model count, last verification, and a safe error.

Verification reads effective credential status and may refresh models. It does not send a billable model completion. Multiple auth files can represent separate accounts; PMux treats them independently and never reads token internals.

## Callback OAuth

```sh
pmux providers login codex --method browser
pmux providers login claude --no-browser --callback-url-stdin
pmux providers login antigravity --no-browser --callback-url-stdin
```

PMux requests the management authorization URL, displays it as selectable non-secret text, polls status, and verifies a usable auth-file record. In a remote/headless session, a callback URL can be read as protected stdin. PMux validates the expected callback route and active state before forwarding it and never logs or persists the pasted URL.

Callback subprocess fallback is disclosed before use because upstream callback helpers may listen on all interfaces. Source-defined ports are Codex 1455, Claude 54545, and Antigravity 51121. Management paste-callback or device authorization is preferred; PMux never automates a login by timed stdin writes.

## Device authorization

```sh
pmux providers login codex --method device --no-browser
pmux providers login kimi --no-browser
```

The active flow may display a verification URI, short-lived user code, expiry, and waiting status because the user must act on them. These values are not retained in logs, state, doctor output, or bundles. Polling respects the server interval, never runs faster than once per second, and has an overall five-minute budget. Canceling requests upstream session cancellation when supported and exits without claiming a credential was created.

## API keys

A key is accepted only through protected TTY input, a private regular file, or dedicated stdin. It is not a positional value or a normal secret-valued option.

```sh
printf '%s\n' "$GEMINI_KEY" | pmux --yes providers login gemini --api-key-stdin
```

The preview contains provider identity, non-secret destination fields, base URL when applicable, and a mask/fingerprint—never the full key. PMux writes through the exact management resource when available or a backed-up, fingerprint-checked structured config transaction. Local/private custom destinations require advanced confirmation. No second full provider-key copy is stored in PMux state.

## Vertex service-account import

```sh
pmux --yes providers login vertex \
  --service-account /absolute/private/path/vertex.json \
  --vertex-prefix team
```

PMux verifies that the source is a private regular JSON file and imports through the Management API first. If unavailable, only the explicit upstream Vertex import flags are permitted. PMux never prints, bundles, modifies, or copies the source service-account content into its state. Success requires a resulting usable credential and model refresh.

## Enable, disable, and remove

```sh
pmux --yes providers enable codex
pmux --yes providers disable codex user@example.com
pmux --yes providers remove codex user@example.com
pmux --yes providers remove openrouter --keep-credentials
```

Enable/disable is idempotent and keeps credentials. Account removal deletes only the selected credential after an exact preview. Provider-scope removal is limited to PMux-recorded configuration and selected managed credentials. PMux never invokes upstream delete-all auth-file behavior and refuses paths outside recorded ownership.

Removing a local OAuth credential does not revoke the provider-side grant. Revoke it separately in the provider account when complete revocation is required.

## Quota state reset

When CLIProxyAPI has cooled down an account after quota errors, an explicit reset clears that state for one auth file:

```sh
pmux providers reset-quota <auth-file-name>
```

The name comes from `pmux providers list`. The reset requires confirmation or global `--yes`. Quota policy itself (`quota-exceeded.switch-project`, `quota-exceeded.switch-preview-model`, `quota-exceeded.antigravity-credits`) is proxy configuration managed with `pmux config --scope proxy set`.

## JSON and streaming events

Global `--json` disables TUI, browser opening, clipboard writes, editor launch, and prompts. OAuth produces redacted NDJSON events: `auth_started`, `verification_required`, `waiting`, and exactly one terminal `complete` or `error`. A verification URL/user code appears only in the transient event needed for authorization; callback URLs and tokens do not.

Noninteractive secret mutations require protected input plus global `--yes`. Invalid/missing input exits with usage error; authentication rejection or no usable credential is an authentication error; canceled sessions are reported as canceled.

## Unsupported channels

PMux manages only provider classes supported by mainline CLIProxyAPI and the closed capability map. An unrecognized login request is rejected rather than converted into a guessed flag. Community-fork-only provider ceremonies are not silently invoked; configure a supported compatibility provider instead.
