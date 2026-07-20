# Doctor, fixes, and diagnostic bundles

`pmux doctor` runs an ordered diagnostic graph. It is read-only unless the canonical `--fix` flag is present. Ordinary doctor makes no release-service request; add `--online` only when release/network checks are intended.

## Run checks

```sh
pmux doctor
pmux doctor --check KEY-SAFEMODE --check SVC-STATE
pmux --json doctor
pmux --json doctor --online
```

Checks cover installation and binary integrity, version/capabilities, absolute config and parsing, private permissions/DACLs, safe mode, service definition/state, port ownership, health, management locality, provider credential status, live model discovery, Claude Code v2 compatibility, and mutation-lock state.

Checks run in dependency order. When a prerequisite prevents evaluation, a dependent result is skipped with that reason rather than reported as a second independent failure. Warnings remain visible but do not change a healthy exit.

A completed run exits 0 when no failed/critical finding remains and 7 when at least one does. If doctor cannot construct results, it uses the global usage, configuration, dependency, authentication, network, ownership, cancellation, or interruption code appropriate to the cause.

## Preview and apply fixes

```sh
pmux doctor --fix KEY-SAFEMODE
pmux --yes doctor --fix KEY-PERMS
pmux doctor --fix
```

A fix is always previewed with affected paths/services, backup and rollback behavior, destructiveness, confirmation, and observable verification. Global `--yes` supplies consent in noninteractive mode; it never bypasses ownership, fingerprint, checksum, protected-input, or verification gates.

Fixes acquire the OS advisory mutation lock only while changing state. Each file/service mutation is journaled. After applying one fix, PMux runs its named verification and re-evaluates downstream checks. A successful write without successful verification is not a successful repair. PMux rolls back where the operation defines a safe inverse and reports both repair and rollback outcomes honestly.

Doctor never reauthenticates a provider, updates a component, deletes a lock file, kills a foreign process, or modifies adopted resources outside a separately confirmed transaction. Non-repairable findings provide canonical manual guidance.

## Safe-mode repair

A placeholder proxy key can make CLIProxyAPI return HTTP 403 with `X-Cpa-Safe-Mode: example-api-key`. `KEY-SAFEMODE` previews a canonical config backup, generation of a cryptographically random replacement, private sidecar update when managed, and authenticated API verification.

The complete key never appears in terminal output, TUI, JSON, logs, clipboard, audit/journal records, or bundles. Only a short mask/fingerprint is retained in PMux state. If hot reload does not activate the change, PMux can restart only its owned service and repeats the same health/authentication verification.

## JSON schema

`pmux --json doctor` emits one object with `checks` and `summary`. Every check has exactly `id`, `status`, `severity`, `summary`, `evidence`, and `repair`. The nested repair has exactly `available`, `description`, `destructive`, `confirmation_required`, and `verification`.

```json
{
  "checks": [
    {
      "id": "CONFIG-ABSOLUTE",
      "status": "pass",
      "severity": "critical",
      "summary": "Service uses an absolute config path",
      "evidence": ["-config", "<absolute-redacted-path>"],
      "repair": {
        "available": false,
        "description": "",
        "destructive": false,
        "confirmation_required": false,
        "verification": "service argv and health re-check"
      }
    }
  ],
  "summary": {
    "passed": 1,
    "warnings": 0,
    "failed": 0,
    "critical": 0,
    "exit_code": 0
  }
}
```

Status values are `pass`, `warn`, and `fail`; unavailable dependent checks may be represented as skipped by the run projection. Evidence is structured as a redacted string array. Obsolete top-level fields such as `name`, `repairable`, and `category` are not part of a check record.

## Diagnostic bundles

```sh
pmux doctor --bundle
pmux --yes doctor --bundle /absolute/private/path/pmux-doctor.tar.gz
pmux --yes doctor --fix KEY-SAFEMODE --bundle /absolute/private/path/pmux-doctor.tar.gz
```

Bundle creation does not imply `--fix` or `--online`. PMux stages allowed data privately, structurally redacts active config, sanitizes terminal control sequences, builds a manifest with archive path/digest/size/source/redaction status, and shows a complete entry preview before exclusive creation.

Allowed data includes the doctor result, version/capability facts, redacted configuration structure/fingerprints, owned service definition/status, bounded redacted logs, provider/auth-file metadata, and model-discovery summary.

The following are unconditionally excluded with no override:

- auth-file bodies and account-bearing raw filenames;
- `api-key.txt`, `secrets.json`, and config backups;
- complete proxy, management, provider, OAuth, callback, or service-account secrets;
- client settings/profile files, SSH material, browser data, environment dumps, and memory dumps.

PMux scans staged entries for known exact values and high-confidence secret patterns. Any unsafe entry is omitted or fails closed; the final archive is private and never overwrites an existing destination. A bundle does not turn failed checks into success: doctor findings still control exit 0/7 unless bundle collection/writing itself has a more specific failure.
