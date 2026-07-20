# Services and foreground operation

PMux uses one lifecycle contract for every native backend. It never relies on the caller's working directory: every core process receives an absolute `-config`, a PMux-owned runtime directory containing no `.env`, and a minimal environment without `PGSTORE_*`, `OBJECTSTORE_*`, or `GITSTORE_*` configuration-store overrides.

## Backends and identities

| Platform | Default | Canonical identity |
|---|---|---|
| Linux | systemd user when usable, otherwise foreground | `pmux-cliproxyapi@<instance-id>.service` |
| macOS | launchd LaunchAgent | `dev.pmux.cliproxyapi.<instance-id>` |
| Windows | foreground | optional task `PMux CLIProxyAPI (<instance-id>)` |
| WSL | Linux systemd user when usable, otherwise foreground | Linux identity |

Windows Task Scheduler is an explicit opt-in convenience, not a Windows Service. PMux creates it with Task Scheduler 2.0 COM and keeps executable and arguments in separate `ExecAction` fields. PMux does not parse localized task-manager output.

Docker-backed adoptions have lifecycle backend `docker-unmanaged`; see [docker.md](docker.md).

## Canonical commands

```sh
pmux service status
pmux service install --start
pmux service start
pmux service start --foreground
pmux service stop
pmux service restart
pmux service logs --source all --lines 100
pmux service uninstall
```

Bare `pmux service` opens the Service TUI on a TTY, behaves as `pmux service status` without a TTY, and produces the same status under global `--json`.

Mutating operations acquire the PMux advisory lock. Stop, restart, install, and uninstall preview their scope and require interactive confirmation or global `--yes` where applicable. PMux refuses to overwrite or remove a foreign definition. `service uninstall` removes only the PMux-owned definition and retains binary, config, keys, auth files, and logs.

Foreground mode exists on every supported platform and is invoked only as:

```sh
pmux service start --foreground
```

It runs attached to the terminal. The proxy stops when that foreground process ends. Duplicate foreground instances are rejected using verified process metadata rather than a stale file alone.

## Health and restart contract

After start or restart, PMux polls `GET /healthz` at most once per second for up to 15 seconds. Each request has a two-second timeout, and no request overlaps the prior one. The first HTTP 200 is lifecycle success.

`X-CPA-VERSION` is optional. Its absence records the core version as `unknown` with a warning; it does not turn a healthy process into a failed start. A running backend that does not answer is reported as running but unhealthy, with at most a bounded redacted log tail and a recommendation to run `pmux doctor`.

Changes to host, port, TLS, token-store backends, and OS-locked plugin libraries require a restart. PMux batches these changes and never reports them active until a restart passes the same health gate.

## Backend details

### systemd user

The unit uses the canonical per-instance name, a PMux service-host executable, the absolute binary/config paths, and `WantedBy=default.target`. It restarts on failure, not after a clean intentional stop. Logs come from the user journal. On a headless system without lingering, PMux provides `loginctl enable-linger $USER` as user-run guidance; it does not change that policy itself.

### launchd

The LaunchAgent plist uses `ProgramArguments`, an explicit `WorkingDirectory`, per-instance state log files, `RunAtLoad`, and restart-on-unsuccessful-exit behavior. PMux manages only a plist carrying its ownership marker and canonical label.

### Windows

Foreground is the default. `pmux service install` explicitly opts into a current-user on-logon Scheduled Task. PMux verifies private DACLs and task identity, avoids elevation, captures logs in PMux-owned files, and prevents overlapping instances.

### WSL

PMux treats the installation as Linux. It uses systemd user only when the distro's user manager and session bus are reachable. It does not mix Windows executable paths or Windows service management into the Linux installation.

## Logs

```sh
pmux service logs [--source pmux|proxy|service|request-error|all] \
  [--level <level>] [--lines <n>] [--since <time>] [--follow]
pmux service logs --source proxy --output /absolute/private/path/proxy.log
pmux --json service logs --follow
```

JSON follow mode emits NDJSON. PMux fetches logs through the management API first, then the recorded native backend, then configured upstream file logs. Every line is escape-sanitized and secret-redacted before display, output, or bundling.

Clearing an upstream log is a distinct confirmed mutation:

```sh
pmux --yes service logs --clear proxy
```

It targets one supported log source and never clears credentials, journal, or audit data.

## Foreign ports and services

PMux displays observed process/service evidence but never offers to terminate an unknown or foreign owner. The available remedies are to use the owning tool or preview a consistent PMux config/service move to another free loopback port. A native artifact without PMux ownership is not overwritten by ordinary service commands; it can change only through the separately confirmed adoption hardening transaction.
