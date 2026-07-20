# v1 acceptance checklist

PMux remains **v0.x** until every §50 criterion and §43 release gate passes. This document tracks what is automated in CI versus what requires a manual run with live credentials.

## Automated gates (CI)

| Gate | Workflow | Evidence |
|---|---|---|
| Unit and integration tests | `test.yml` | `go test ./...` on Linux amd64/arm64, Darwin amd64/arm64, Windows amd64 |
| Lint and depguard | `lint.yml` | golangci-lint v2 |
| Security, privacy, zero egress | `security-privacy.yml` | govulncheck, telemetry scan, strace loopback-only startup |
| CLIProxyAPI compatibility | `compat.yml` | Checksum-verified v7.2.91/v7.2.92 assets + `TestRealCoreContract` |
| Native platform lifecycle | `platform-e2e.yml` | `platform_e2e` tagged tests (systemd, launchd, Windows task, foreground) |
| WSL awareness | `wsl.yml` | `wsl_e2e` tagged tests inside WSL on Windows runners |
| Release artifact matrix | `release.yml` on signed `v*` tag | GoReleaser five-target build, SPDX SBOMs, checksum verification |

Run the full automated stack locally before tagging:

```sh
go test ./...
PMUX_RELEASE_E2E=1 go test -tags=platform_e2e ./...
PMUX_RELEASE_E2E=1 go test -tags=wsl_e2e ./...   # inside WSL only
go test -tags=compat -run TestRealCoreContract ./internal/adapter/mgmtapi/...  # requires live core env
```

## Manual gates (live credentials)

These §50 criteria cannot run in hermetic CI without disposable provider secrets and Claude Code installed on each target:

| Criterion | Requirement |
|---|---|
| AC-1 | `pmux setup --mode managed` through provider auth, model pick, and Claude completion on each v1 OS/arch + WSL |
| AC-4 | Full provider matrix: callback OAuth, device OAuth, API keys, Vertex import, xAI mechanism probe |
| AC-6 | Live `pmux launch --client claude --model <id>` with Claude Code ≥2.0.0 |

Use the provider acceptance workflow (`.github/workflows/provider-acceptance.yml`) with repository secrets, or run the commands in `docs/providers.md` on each platform and attach redacted transcripts to the release issue.

## Tagging v1.0.0

1. Confirm `Release candidate gates` workflow is green on `main`.
2. Complete the manual provider matrix on at least one Linux amd64 host (extend to all targets before public v1.0).
3. Create a **signed** annotated tag: `git tag -s v1.0.0 -m "PMux v1.0.0"`.
4. Push the tag; `release.yml` runs every gate again and publishes GitHub Release assets.

Cosign artifact signing is deferred per spec §37; trust anchor is checksum-verified GitHub Releases plus signed git tags.
