# Contributing to PMux

PMux is an MIT-licensed, terminal-first companion to CLIProxyAPI. The repository is currently pre-v1. Contributions must preserve the public grammar, native-platform scope, privacy rules, and phase boundaries described in the repository documentation.

By submitting a contribution, you agree to license it under the project's MIT License. PMux requires neither a contributor license agreement nor a Developer Certificate of Origin.

## Before opening a pull request

1. Search [open issues](https://github.com/0p9b/pmux/issues) and keep the change focused.
2. For security-sensitive findings, do not open a public issue; follow [SECURITY.md](SECURITY.md).
3. Use the pinned Go toolchain from `go.mod` (currently the `go1.26.5` toolchain directive).
4. Reuse the architecture boundaries already present. Do not create a second error, configuration, management, or service convention.

## Architecture boundaries

- `cmd/pmux` is the composition root and Cobra command tree.
- `internal/tui` is presentation only and performs no direct filesystem, HTTP, subprocess, or service I/O.
- `internal/app` owns use-case sequencing.
- `internal/domain` owns policies and ports; it does not import adapters or presentation code.
- `internal/adapter` owns side effects. Management route strings stay in `internal/adapter/mgmtapi`; YAML AST operations stay in `internal/adapter/configfile`; core subprocesses stay in `internal/adapter/subproc`.
- `internal/pmuxerr` is the only foundation error representation.
- PMux never reimplements provider protocols, token refresh, model routing, or API translation.

## Public contract rules

Command examples and user-visible changes must use the canonical command tree. In particular:

- setup is `pmux setup --mode managed|adopt`;
- provider operations are under `pmux providers`;
- model operations are under `pmux models`;
- Claude launch is `pmux launch --client claude --model <id>` or `pmux claude <id>`;
- repairs use `pmux doctor --fix` and bundles use `pmux doctor --bundle`;
- PMux and proxy configuration are separated by `pmux config --scope pmux|proxy`;
- updates are manual: `pmux update check|self|proxy`.

Do not add undocumented aliases, no-op future commands, shell-secret export paths, static model catalogs, automatic update checks, or telemetry. Claude Code is the only v1 client. Additional clients and named profiles belong to phase 2; multi-host fleet work belongs to phase 3. Docker remains detect/adopt/diagnose-only in v1.

## Privacy and security requirements

A change must not expose a complete proxy key, management key, provider API key, OAuth token, callback URL, or service-account content through terminal output, TUI state, JSON, logs, errors, audit/journal records, clipboard, or bundles. Auth-file contents are never bundled. Secret input belongs in protected TTY input, a private file, or a dedicated stdin option—not a positional argument or ordinary secret-valued flag.

Managed launches keep management on loopback, use an absolute core config path, run from a PMux-owned directory without `.env`, and scrub configuration-store environment variables. Network activity must follow an explicit user action. Any new egress, secret, config, permission/ACL, service, or update-supply-chain behavior needs focused tests and explicit review.

## Build and focused verification

Use narrow package commands while developing:

```sh
go test ./internal/domain/...
go test ./internal/app/...
go test ./internal/adapter/mgmtapi/...
go test ./cmd/pmux/...
go run ./cmd/pmux version
go run ./cmd/pmux --json doctor
```

Before requesting review, run the repository gates applicable to the change:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Also run target-specific integration commands documented by the relevant workflow when changing Linux services, launchd, Windows Task Scheduler/DACLs, WSL behavior, real-core compatibility, updates, or release code. Cross-compilation alone is not native acceptance evidence.

Tests should defend observable behavior: validation boundaries, lifecycle transitions, exact argv/environment, rollback, redaction, parity, and real error handling. Keep the default suite hermetic; live provider credentials and ordinary internet access do not belong in it.

## Documentation and generated interfaces

Update user documentation for any visible change. Every TUI operation must have the same application-layer behavior through a canonical CLI command and JSON or NDJSON representation. Command examples must be executable under the public grammar. Never include a complete real-looking secret.

Do not claim v1 support or acceptance based only on implementation, compilation, a narrowed test, or a release archive. Native OS/architecture, service, WSL, provider, privacy, and Windows ACL evidence is release-gating.

## Pull requests

A pull request should:

- explain the user-visible problem and the chosen solution;
- remain focused and avoid unrelated refactors;
- add behavior coverage for a new observable contract or plausible regression;
- update docs and generated command reference when public behavior changes;
- include a changelog fragment when the repository's changelog process requires one;
- state which commands and native scenarios were actually run;
- answer the pull-request security/privacy checklist accurately.

All required CI must be green. Reviewers may request a smaller change when scope obscures the security or lifecycle contract. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community expectations.
