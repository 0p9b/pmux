## Summary

<!-- Explain the user-visible problem and the focused solution. -->

## Verification

<!-- List the exact commands and native scenarios actually run. Do not imply unexecuted acceptance. -->

- [ ] Focused behavior tests pass.
- [ ] Applicable package/integration tests pass.
- [ ] I listed native OS/architecture/service/WSL/provider scenarios that were actually exercised and marked anything not run.

## Public contract

- [ ] User-visible commands and examples follow the canonical PMux command grammar.
- [ ] Every operational TUI change has equivalent CLI and JSON or NDJSON behavior through the same application use case.
- [ ] Documentation and generated command reference are updated when public behavior changed.
- [ ] A new observable contract has behavior coverage for a plausible regression.
- [ ] This change respects v1/phase-2/phase-3 and Docker lifecycle boundaries.

## Architecture and maintenance

- [ ] The change respects package boundaries (`domain` ports, `app` sequencing, adapter side effects, presentation-only TUI, shared `pmuxerr`).
- [ ] The change is focused and contains no unrelated refactor or compatibility shim.
- [ ] A changelog fragment is included when required by the repository's changelog process.

## Security and privacy review

Check **Yes** only when the change affects the area, then explain below.

| Area | Yes | No |
|---|:---:|:---:|
| Secrets, credentials, callback/device data, or redaction | [ ] | [ ] |
| Network egress, provider/release requests, or telemetry boundary | [ ] | [ ] |
| Proxy/PMux/client configuration, backups, rollback, or ownership | [ ] | [ ] |
| Unix permissions or Windows DACLs | [ ] | [ ] |
| Native service definitions, subprocess argv/environment, or WSL | [ ] | [ ] |
| Download/checksum/extraction/update supply chain | [ ] | [ ] |
| Diagnostic logs, JSON, audit/journal, or bundles | [ ] | [ ] |

### Security/privacy explanation

<!-- For every Yes, describe threats considered, redaction/ownership/rollback behavior, and focused evidence. Never paste a complete secret or auth-file content. -->

- [ ] No complete proxy, management, provider, OAuth, callback, service-account, or SSH secret appears in code examples, fixtures, output, logs, JSON, audit/journal records, or attachments.
- [ ] Auth-file contents remain excluded from diagnostic bundles with no override.
- [ ] Any new network activity requires an explicit user action; this change adds no telemetry, analytics, crash upload, heartbeat, background version check, or automatic update.
- [ ] Foreign/adopted files, services, processes, and containers remain unchanged outside a separately confirmed ownership transaction.

## Release evidence

- [ ] I do not claim v1 or native target acceptance from compilation, cross-compilation, a narrowed test, or archive publication alone.
- [ ] If this affects a release gate, I linked the captured native evidence or clearly stated that the gate remains pending.
