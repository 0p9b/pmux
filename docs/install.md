# Install, managed setup, and adoption

PMux is currently pre-v1. The procedures below are the intended v1 contract; published source or an archive does not imply that every native acceptance gate has completed.

## Supported targets and roots

The v1 target set is Linux amd64/arm64, macOS amd64/arm64, Windows amd64, and WSL-aware Linux installations.

| Root | Linux | macOS | Windows |
|---|---|---|---|
| Config | `$XDG_CONFIG_HOME/pmux`, default `~/.config/pmux` | `~/Library/Application Support/PMux` | `%APPDATA%\PMux` |
| State | `$XDG_STATE_HOME/pmux`, default `~/.local/state/pmux` | `~/Library/Application Support/PMux/State` | `%LOCALAPPDATA%\PMux\State` |
| Cache | `$XDG_CACHE_HOME/pmux`, default `~/.cache/pmux` | `~/Library/Caches/PMux` | `%LOCALAPPDATA%\PMux\Cache` |
| Data | `$XDG_DATA_HOME/pmux`, default `~/.local/share/pmux` | `~/Library/Application Support/PMux` | `%LOCALAPPDATA%\PMux\Data` |

`--config-dir <path>` overrides only the PMux config root. It does not relocate data, state, cache, or the core auth directory.

## Install PMux

### Verified release archive

Download the target archive and `checksums.txt` from the same [PMux release](https://github.com/0p9b/pmux/releases). Verify the archive bytes under their exact filename before extraction:

```sh
sha256sum --check --ignore-missing checksums.txt
```

On macOS use `shasum -a 256 -c checksums.txt`. On Windows compare `Get-FileHash -Algorithm SHA256` with the matching `checksums.txt` entry before `Expand-Archive`. A missing, duplicated, malformed, or different digest is a hard failure. Do not extract or execute that archive.

After verification, extract `pmux` or `pmux.exe` and place it on `PATH` using the owning platform's normal file-management mechanism.

### Build from the tagged module

```sh
go install github.com/0p9b/pmux/cmd/pmux@latest
pmux version
```

Use the toolchain required by `go.mod`. This source build is distinct from verifying a published archive.

## Managed setup

```sh
pmux setup --mode managed
```

Managed setup previews, confirms, executes, and verifies each mutation. It:

1. selects the CLIProxyAPI release compatible with the host (supported floor 7.2.91; managed default 7.2.92);
2. downloads the exact archive and upstream `checksums.txt`;
3. verifies SHA-256 before extraction, then validates executable format and architecture;
4. installs the immutable binary under `<data>/cli-proxy-api/versions/<version>/` and selects it through `current`;
5. creates `<data>/instances/<id>/{config.yaml,api-key.txt,auth/,runtime/}` with private permissions;
6. generates distinct cryptographically random proxy and management secrets without displaying them;
7. binds the proxy to loopback, keeps management local, disables the unused management panel, and explicitly enables `ws-auth`;
8. starts the platform backend and verifies `/healthz` plus authenticated local API reachability;
9. interactively offers provider authentication, live model selection, and Claude Code launch.

A healthy core can be retained when the user deliberately skips onboarding. PMux then reports setup as incomplete and prints only canonical next actions:

```sh
pmux providers login <provider>
pmux models list --refresh
pmux launch --client claude --model <exact-discovered-id>
```

Noninteractive deterministic setup requires confirmation:

```sh
pmux --yes setup --mode managed
```

It never guesses a provider, account, or model.

## Read-only adoption

Adoption has two separate transactions. Import is read-only with respect to the candidate:

```sh
pmux setup --mode adopt \
  --proxy-path /absolute/path/to/cli-proxy-api \
  --config-path /absolute/path/to/config.yaml
```

PMux resolves and validates the binary, config, auth directory, process/listener, service, and Docker evidence. It writes only its own adoption record. It does not move, chmod, start, stop, rewrite, or rotate candidate files during import.

Version detection uses a running `X-CPA-VERSION`, then checksum-bound package metadata, then an isolated temporary-config banner probe. The probe never starts the user's config or auth directory. When no safe method succeeds, version remains `unknown` and feature probes control availability.

Hardening is a new invocation with a complete preview and backup plan:

```sh
pmux --yes setup --mode adopt \
  --proxy-path /absolute/path/to/cli-proxy-api \
  --config-path /absolute/path/to/config.yaml \
  --harden
```

Without both `--harden` and global `--yes`, noninteractive hardening is refused. An accepted transaction changes only the previewed config, key, permissions, or service artifacts and verifies health/authentication; a failed transaction restores recoverable artifacts.

## WSL rules

A WSL install is Linux-local. Managed data must live inside the distribution, not on a Windows-mounted filesystem where PMux cannot guarantee private Unix permissions. PMux uses WSL-aware browser guidance, preserves Linux paths for Linux processes, and selects systemd user only when the user manager/session bus are reachable; otherwise it uses foreground mode.

## Removing PMux-managed lifecycle artifacts

The public service removal operation is:

```sh
pmux service uninstall
```

It removes only the PMux-owned native service definition and retains binary, config, keys, auth files, and logs. Remove the `pmux` executable with the same mechanism that installed it. Before manually removing retained state or data, create and inspect backups and verify that no adopted or foreign path is in scope. PMux does not silently delete provider credentials or adopted installations.

## Updates

Updates are manual:

```sh
pmux update check
pmux update self
pmux update proxy
```

`pmux update proxy` applies only to managed CLIProxyAPI installations. Adopted and Docker-owned cores must be updated with their owning mechanism. Every download is checksum-verified before extraction; cutover is journaled and rolls back the selected core when health or authenticated API verification fails. An empty model list is a warning when no effective credential exists, not by itself a rollback reason.
