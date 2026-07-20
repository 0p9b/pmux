# Security and privacy model

PMux runs as the current user and orchestrates a local CLIProxyAPI. It is not a credential vault or a network proxy. Root/SYSTEM compromise, physical compromise, and hostile processes already executing as the same user are outside its isolation boundary; those actors can inspect either private files or a launched client's environment.

Report vulnerabilities privately through [GitHub Private Vulnerability Reporting](https://github.com/0p9b/pmux/security/advisories/new), following [SECURITY.md](../SECURITY.md).

## Protected assets

| Asset | Canonical location and PMux handling |
|---|---|
| Proxy API key | Full value only in core `config.yaml` and managed `api-key.txt`; PMux state stores path, mask, and fingerprint |
| Management secret | Plaintext in private PMux `secrets.json`; the core persists its bcrypt hash |
| OAuth tokens | CLIProxyAPI auth directory; PMux uses management status metadata and does not copy token bodies |
| Provider API keys | Private core configuration; accepted through protected input and never echoed |
| Service-account input | User-owned private JSON source; validated/imported without display or state copy |
| Backups/journal/audit | Private PMux state root; backups can contain restorable secrets and are never bundled |
| SSH material | User-owned SSH agent/config for later multi-host work; PMux does not copy private keys |

On Unix private directories are mode 0700 and secret-bearing files are 0600. On Windows PMux disables inheritance and grants Full Control only to the current-user SID and SYSTEM, with inheritable directory ACEs; it reads the DACL back and fails closed if the invariant cannot be proved.

## Network boundaries

Managed proxy configuration binds `127.0.0.1`, uses a real random proxy key, explicitly enables `ws-auth`, disables the unused management panel, and keeps `remote-management.allow-remote` false. Management access remains localhost-only even if an adopted proxy was intentionally exposed. Later remote management uses SSH loopback forwarding rather than a routable management endpoint.

A PMux-initiated non-loopback proxy bind is an advanced transaction requiring readable TLS certificate/key paths, at least one non-placeholder proxy key, a complete redacted diff and exposure preview, and typed `EXPOSE` confirmation. Management still remains local. Read-only adoption reports existing exposure without silently rewriting it.

Ordinary startup, cached status, local configuration reads, service status/logs, offline doctor, and bundle creation have no non-loopback egress. Explicit install, provider authentication/verification, model refresh/test, `pmux doctor --online`, update actions, and launched clients are network-capable. PMux has no telemetry, analytics, crash reporting, heartbeat, or automatic update check.

## Process and configuration isolation

Every PMux-started core receives:

- an absolute executable and `-config <absolute-path>` argv pair;
- a PMux-owned runtime directory containing no `.env`;
- a minimal allowlisted environment;
- no `PGSTORE_*`, `OBJECTSTORE_*`, `GITSTORE_*`, `MANAGEMENT_PASSWORD`, or inherited provider/client secret families.

Subprocess argv is constructed without a shell. Provider fallback flags come from a closed source-verified map. PMux never derives a flag, parses login prose, or automates timed stdin. The only human-readable core output parsed is the isolated temporary-config startup banner used as a last-resort version probe.

Configuration mutations validate semantic scope, reject placeholders, fingerprint concurrent state, create a canonical private backup, write atomically when PMux owns the file transport, and verify observable state. Management API writes use a prior-payload backup, pre-write compare, PUT, re-GET verification, and compensating PUT; PMux does not claim upstream file atomicity.

Mutations acquire an OS advisory lock (`flock` or `LockFileEx`) and append a redacted journal. There is no force-unlock path. PMux never kills a foreign port owner or overwrites a foreign service definition.

## Secret policy

Complete proxy, management, provider, OAuth, callback, and service-account secrets never appear in the TUI, terminal output, JSON, errors, verbose traces, logs, clipboard, audit/journal records, generated shell text, or diagnostic bundles. Values are omitted or masked to at most their first seven and last four characters; short values are fully masked.

PMux applies both pattern-based redaction and a transient exact known-value set before any output sink. It strips ANSI/OSC and unsafe control characters from upstream text to prevent terminal spoofing or clipboard escape attacks. Secret-bearing types format only as masks, and retained buffers are overwritten on a best-effort basis where PMux still owns them; Go cannot guarantee complete memory erasure.

Auth-file contents, `api-key.txt`, `secrets.json`, config backups, client settings, SSH material, environment dumps, and memory dumps are never diagnostic-bundle candidates. There is no override. See [doctor.md](doctor.md) for the bundle manifest and preview contract.

## Download and update trust

PMux and CLIProxyAPI release archives are fetched over HTTPS and matched against the exact release filename in `checksums.txt` before extraction. Missing, malformed, ambiguous, or mismatched entries fail closed. Verified archives are then checked for path traversal, special files, expected executable magic, and target architecture before staging.

Checksums and archives share GitHub Releases as the trust anchor; when an upstream project publishes no independent signature, PMux does not claim the checksum proves protection from a compromised release account. Updates remain manual and retain a verified rollback target.

## Threat summary

PMux specifically mitigates malicious config/base URLs, archive substitution in transit, other-user file reads, terminal-control text from providers, shell-history leakage, exposed callback listeners, diagnostic leakage, accidental network exposure, wide SSH authority, and lifecycle/config rollback failures.

Residual risks include same-user process inspection of `ANTHROPIC_AUTH_TOKEN` while Claude runs, provider-side OAuth grants that local credential removal cannot revoke, novel secret shapes in otherwise permitted logs, and compromise of the shared release origin. User preview and provider-side revocation remain necessary controls for those cases.
