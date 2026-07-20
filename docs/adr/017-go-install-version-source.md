# ADR-017: Support release and go-install version metadata

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

GoReleaser can inject deterministic version, commit, and date values through linker flags. A binary installed with `go install github.com/0p9b/pmux/cmd/pmux@<version>` does not run that GoReleaser pipeline, but Go embeds module and VCS build information. Treating tagged source installs as `dev` would make compatibility and diagnostics misleading.

## Decision

`internal/version` gives nonempty GoReleaser linker variables precedence. When they are absent it reads `runtime/debug.ReadBuildInfo`: `Main.Version` supplies a tagged module version and `vcs.revision`/`vcs.time` supply commit and date when present. Only an untagged or metadata-free development build reports `dev`. Version reporting never performs network I/O.

CI verifies both an archive-style linker-flag build and the module build-information path.

## Consequences

GitHub Release archives and `go install` binaries report useful, consistent identity without generated source or a network lookup. Reproducible archives keep explicit linker metadata, while local development remains clearly distinguished. The final module path is permanently `github.com/0p9b/pmux` and is used by release linker flags and install examples.
