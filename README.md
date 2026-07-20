# PMux

[![Release candidate gates](https://github.com/0p9b/pmux/actions/workflows/release-candidate.yml/badge.svg)](https://github.com/0p9b/pmux/actions/workflows/release-candidate.yml)
[![Lint](https://github.com/0p9b/pmux/actions/workflows/lint.yml/badge.svg)](https://github.com/0p9b/pmux/actions/workflows/lint.yml)
[![Tests](https://github.com/0p9b/pmux/actions/workflows/test.yml/badge.svg)](https://github.com/0p9b/pmux/actions/workflows/test.yml)
[![Security and privacy](https://github.com/0p9b/pmux/actions/workflows/security-privacy.yml/badge.svg)](https://github.com/0p9b/pmux/actions/workflows/security-privacy.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**One terminal interface for providers, models, and coding agents — the terminal control plane for CLIProxyAPI.**

> **Pre-release status:** PMux is currently v0.x. Automated release gates (test matrix, lint, security/privacy, CLIProxyAPI compatibility, native platform E2E, and WSL acceptance) run on every push to `main`. Full **v1.0** also requires the manual provider and Claude Code checklist in [docs/acceptance.md](docs/acceptance.md).

PMux is a terminal-first companion for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It installs or adopts a core, keeps its management API local, guides provider authentication, discovers models from the running core, launches Claude Code with process-scoped credentials, and diagnoses failures. PMux does not proxy model traffic itself.

## First run

A normal interactive session continues from setup to the first agent without asking the user to edit YAML or copy a complete secret:

```text
$ pmux setup --mode managed
✓ CLIProxyAPI archive verified before extraction
✓ Secure loopback configuration created
✓ Service healthy
Connect a provider now? yes
✓ Provider authenticated
Select a model: <exact dynamically discovered ID>
Launch Claude Code now? yes
Launching Claude Code through http://127.0.0.1:8317…
```

This transcript describes the intended accepting flow; it is not a claim that every v1 native acceptance run has completed.

## Install

### GitHub Release archive

1. Open the [PMux releases page](https://github.com/0p9b/pmux/releases) and download the archive for one supported target plus that release's `checksums.txt`.
2. Verify the archive **before** extracting it. Substitute the exact downloaded filename:

   ```sh
   sha256sum --check --ignore-missing checksums.txt
   ```

   On macOS:

   ```sh
   shasum -a 256 -c checksums.txt
   ```

   On Windows PowerShell, compare `(Get-FileHash .\pmux_<version>_windows_amd64.zip -Algorithm SHA256).Hash` with the matching entry in `checksums.txt` before using `Expand-Archive`.
3. Stop if the entry is absent or the digest differs. Extract only verified bytes, then place `pmux` (or `pmux.exe`) on `PATH`.

Release assets target Linux amd64/arm64, macOS amd64/arm64, and Windows amd64. A published archive is not by itself proof that all v1 acceptance gates have passed; consult the release notes.

### Go install

With the toolchain required by `go.mod`:

```sh
go install github.com/0p9b/pmux/cmd/pmux@latest
pmux version
```

This builds from the tagged module source. It does not verify a release archive; use the archive procedure when you require the published archive checksum.

## Quickstart

```sh
pmux setup --mode managed
pmux providers login codex
pmux models list --refresh
pmux launch --client claude --model <exact-discovered-id>
```

For noninteractive status and discovery, put the global flag before the command:

```sh
pmux --json
pmux --json models list --refresh
pmux --json doctor
```

Managed setup is the new-user default. Read-only import of an existing installation uses:

```sh
pmux setup --mode adopt --proxy-path /absolute/path/to/cli-proxy-api \
  --config-path /absolute/path/to/config.yaml
```

Changing an adopted installation is a separate transaction and requires an explicit hardening preview. In noninteractive use it requires both `--harden` and global `--yes`.

## Supported v1 target contract

| Target | Lifecycle contract |
|---|---|
| Linux amd64/arm64 | systemd user when available; otherwise foreground |
| macOS amd64/arm64 | launchd LaunchAgent; foreground also available |
| Windows amd64 | foreground by default; Task Scheduler is explicit opt-in |
| WSL | Linux installation with WSL-aware browser, filesystem, and service guidance |

Foreground mode is always explicit:

```sh
pmux service start --foreground
```

Docker support in v1 is limited to detection, read-only adoption, and diagnosis of an existing CLIProxyAPI container. PMux does not manage container lifecycle.

## Features and commands

| Task | TUI | Canonical CLI |
|---|---|---|
| Local readiness | Dashboard | `pmux` or `pmux --json` |
| Managed or read-only setup | First-run flow | `pmux setup --mode managed|adopt` |
| Providers and accounts | Providers | `pmux providers list`, `pmux providers login <provider>`, `pmux providers verify [provider]` |
| Live model catalog | Models | `pmux models list --refresh`, `pmux models test <model>` |
| Claude Code launch | Launch | `pmux launch --client claude --model <id>` or `pmux claude <id>` |
| Diagnostics and repair | Doctor | `pmux doctor`, then an explicit `pmux doctor --fix [<id>...]` |
| Native lifecycle and logs | Service, Logs | `pmux service status`, `pmux service logs [--follow]` |
| Proxy/PMux configuration | Config, Settings | `pmux config --scope proxy|pmux show|get|set|edit|backup|restore` |
| Manual updates | — | `pmux update check`, `pmux update self`, `pmux update proxy` |

Claude Code v2.0.0 or newer is the only v1 coding client. PMux passes one exact live model ID and adds only `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` to the child process. Codex CLI, Gemini CLI, OpenCode, named profiles, and fallback chains are phase 2. Multi-host fleet work is phase 3 and is not exposed as a v1 command.

## Privacy and security

- PMux has no telemetry, analytics, crash upload, background version check, or automatic update. Network access occurs only for an explicit install, provider authentication or verification, model test/refresh, `pmux doctor --online`, update action, or launched client.
- Complete proxy, management, provider, OAuth, and service-account secrets are never printed, returned in JSON, copied, logged, or placed in diagnostic bundles. Auth-file contents are always excluded.
- Managed instances bind the proxy to loopback, keep management localhost-only, explicitly enable `ws-auth`, use random keys, and pass an absolute config path from a PMux-owned runtime directory.
- Provider model IDs come from the running core. PMux ships no static fallback catalog.
- Release archives are verified against `checksums.txt` before extraction; PMux has no automatic updater.

See [Security](docs/security.md) and [private vulnerability reporting](SECURITY.md).

## Documentation

- [Installation and adoption](docs/install.md)
- [Services](docs/services.md)
- [Providers](docs/providers.md)
- [Claude Code](docs/clients.md)
- [Model discovery](docs/models.md)
- [Doctor and diagnostic bundles](docs/doctor.md)
- [Docker boundary](docs/docker.md)
- [Security model](docs/security.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Accessibility and CLI/JSON parity](docs/accessibility.md)
- [Future fleet design](docs/fleet.md)

Contributions are welcome under the [contribution guide](CONTRIBUTING.md), [Code of Conduct](CODE_OF_CONDUCT.md), and [MIT License](LICENSE). Report security problems privately as described in [SECURITY.md](SECURITY.md).
