# Troubleshooting

Start with read-only diagnostics from any working directory:

```sh
pmux doctor
pmux service status
pmux service logs --source all --lines 100
```

Add global `--json` for automation. Use `pmux doctor --online` only when release/network checks are intentionally needed. Do not post unreviewed logs or bundles publicly; see [doctor.md](doctor.md) and [SECURITY.md](../SECURITY.md).

## Core looked for `config.yaml` in the wrong directory

Observed upstream failure:

```text
failed to read config file: open /home/user/config.yaml: no such file or directory
```

Without an explicit config argument, CLIProxyAPI may resolve `config.yaml` relative to its working directory. It can also read `.env` from that directory, while `PGSTORE_*`, `OBJECTSTORE_*`, and `GITSTORE_*` variables can redirect configuration selection.

PMux-owned services and subprocesses always pass an absolute config path, use a PMux-owned runtime directory without `.env`, and scrub those store-variable families. Inspect the recorded definition:

```sh
pmux doctor --check CFG-CWD
pmux service status
```

For an adopted foreign definition, read-only import does not rewrite it. Run a separate interactive adoption hardening transaction and review the exact service backup/cutover before confirming.

## HTTP 403 and safe mode

A template/example proxy key can make CLIProxyAPI reject protected requests with HTTP 403, `X-Cpa-Safe-Mode: example-api-key`, and `unsafe_example_api_key`.

```sh
pmux doctor --check KEY-SAFEMODE
pmux doctor --fix KEY-SAFEMODE
```

The fix previews a backup and random-key replacement, then verifies authenticated `/v1/models` access without printing the new complete key. If verification fails, PMux reports rollback state; it does not claim the repair succeeded because a write completed.

## Management API returns 404

The Management API route tree may be absent when no management secret enables it. An individual feature route may also be unavailable on a particular core. PMux first confirms `/healthz`, the recorded config, core version or `unknown`, and exact endpoint probe before naming the cause.

```sh
pmux doctor --check MGMT-LOCAL
pmux version
```

Do not expose management remotely to solve a local configuration problem. A managed or hardened instance keeps `remote-management.allow-remote` false.

## Management authentication failed or is temporarily banned

CLIProxyAPI can ban an IP for 30 minutes after five failed management-auth attempts. PMux stops after one 401; it does not cycle candidate secrets.

Wait out a confirmed ban window. If PMux's stored secret and the core no longer match, use the management-key repair offered by doctor. The repair generates and re-synchronizes a new secret through a backed-up config transaction without displaying it.

## Port already in use

```sh
pmux doctor --check NET-PORT
```

Doctor reports the owner evidence available on the platform. PMux never offers to terminate or signal an unknown or foreign process. Stop it with its owning tool or preview a consistent PMux-owned config/service move to another free loopback port.

## Service runs but does not answer

After start/restart PMux polls `/healthz` no faster than once per second for up to 15 seconds, with a two-second timeout per request. A missing `X-CPA-VERSION` is only a version-unknown warning. A backend process without HTTP 200 is unhealthy.

```sh
pmux service logs --source service --lines 100
pmux doctor --check SVC-STATE --check NET-HEALTH
```

Check the absolute config path, runtime CWD, port owner, private-file access, and the last redacted core lines. PMux avoids unbounded restart loops.

## OAuth callback cannot reach the host

Source-defined callback ports are Codex 1455, Claude 54545, and Antigravity 51121. Upstream subprocess fallback listeners can bind all interfaces even in no-browser mode.

Prefer Management API paste-callback or device authorization:

```sh
pmux providers login codex --method device --no-browser
pmux providers login claude --no-browser --callback-url-stdin
```

PMux prints a selectable verification URL and, for paste flow, reads the full callback as protected stdin. It validates state and never logs/persists that URL. SSH tunnel guidance is optional troubleshooting, not a requirement of the management paste path. On WSL, PMux first checks Windows-browser reachability and otherwise recommends device or paste behavior.

## OAuth timed out or was canceled

OAuth has an overall five-minute budget and polling no faster than once per second. Re-run the same canonical provider login command to create a fresh session. PMux requests upstream session cancellation when supported and verifies a new usable credential rather than parsing success prose.

## No models are listed

```sh
pmux providers verify
pmux models list --refresh
pmux doctor --check MOD-CATALOG
```

A successful empty list is valid when no effective credentials serve models. PMux distinguishes no credentials, safe mode, unusable credentials, and missing attribution capability. It does not fabricate model IDs. A stale cache is display-only; launch requires live model availability.

## Model attribution is unknown

When management model-attribution endpoints are absent or unusable, PMux falls back to `/v1/models`. Its `owned_by` value is vendor-level, not a verified provider channel, so `Unknown` is honest. Update the managed core only through an explicit `pmux update proxy` if a measured compatibility gate recommends it.

## Claude Code does not launch

```sh
pmux doctor --check CLI-CLAUDE
pmux models list --refresh
```

PMux requires Claude Code v2.0.0 or newer with parseable version output. The exact selected ID must be live, the working directory accessible, and passthrough arguments must not contain another `--model`. PMux does not install Claude Code or guess older client mappings.

## Update failed

```sh
pmux update check
pmux update proxy
```

A missing/mismatched archive checksum fails before extraction or cutover. After cutover, health and authenticated API failures restore the prior selected version when rollback is safe. Review `pmux service logs` and `pmux doctor`; do not bypass verification. Docker-backed and adopted cores must be updated with their owning installation mechanism.

## Create a private diagnostic bundle

```sh
pmux doctor --bundle
```

Review the manifest and redacted staged entries before confirmation. Auth-file bodies and complete secrets are excluded unconditionally, but automated redaction cannot recognize every novel secret format. Keep the archive private and use [private vulnerability reporting](https://github.com/0p9b/pmux/security/advisories/new) for security defects.
