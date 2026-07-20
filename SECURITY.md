# Security Policy

## Supported versions

PMux is currently pre-v1. The latest published v0.x line receives security fixes while the project completes the native acceptance matrix. Older development snapshots may be asked to upgrade before a report can be reproduced. Release notes at [github.com/0p9b/pmux/releases](https://github.com/0p9b/pmux/releases) identify supported versions and any necessary backports.

## Report a vulnerability privately

Do not disclose suspected vulnerabilities in a public issue, discussion, pull request, log paste, or diagnostic attachment.

Use [GitHub Private Vulnerability Reporting](https://github.com/0p9b/pmux/security/advisories/new). Include only the minimum information needed to reproduce the problem:

- affected PMux version, OS, architecture, and installation mode;
- affected CLIProxyAPI version or `unknown`;
- service backend and canonical PMux command;
- impact, preconditions, and reproducible steps;
- redacted evidence or a minimal proof of concept.

Never include live provider credentials, OAuth tokens, management keys, proxy keys, callback URLs, service-account files, auth-file contents, SSH private keys, or an unreviewed diagnostic bundle. Use unique disposable canaries if secret-shaped input is essential to the reproduction.

Maintainers target acknowledgment within 72 hours and a critical fix within 14 days. These are response targets rather than guarantees. We will coordinate validation, remediation, release timing, and disclosure through the private advisory. Please allow a reasonable remediation period before public disclosure.

## Scope

Reports are in scope when they affect:

- PMux source or release artifacts;
- release download, checksum, extraction, self-update, or proxy-update behavior;
- secret redaction, credential handling, logs, JSON, clipboard, journals, audit records, or bundles;
- configuration transactions, ownership checks, backups, rollback, or mutation locking;
- Unix modes or Windows private DACL creation and verification;
- systemd user, launchd, foreground, Task Scheduler, WSL, or subprocess isolation;
- management API locality/authentication, provider login orchestration, or callback handling;
- unexpected egress, telemetry, analytics, crash upload, or background update behavior;
- dependencies and the build/release supply chain.

CLIProxyAPI vulnerabilities that do not arise from PMux integration are coordinated with and redirected to the [CLIProxyAPI project](https://github.com/router-for-me/CLIProxyAPI/security). Do not publish exploit details while that coordination is in progress.

## Security invariants

PMux is designed around the following release-blocking rules:

- managed management access remains localhost-only;
- release archives are checksum-verified before extraction;
- complete secrets do not appear in PMux output or diagnostic artifacts;
- auth-file contents are never copied into bundles;
- normal/offline behavior has no unrequested outbound traffic;
- no telemetry, analytics, crash beacons, or automatic updates are permitted;
- foreign files, services, containers, and processes are not modified without a separately confirmed ownership transaction.

The full model and residual risks are documented in [docs/security.md](docs/security.md). Root/SYSTEM compromise and processes already running as the same user are outside PMux's isolation boundary, but PMux still minimizes copies and lifetime of secret material.
