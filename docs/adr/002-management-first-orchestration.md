# ADR-002: Use management-first orchestration

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

CLIProxyAPI exposes structured management routes, `/healthz`, `/v1/models`, configuration files, and a finite set of login/import flags. Its management API has no published stability guarantee, and the executable has no version flag. Parsing ordinary human output would make control flow locale- and wording-dependent.

## Decision

Every operation uses this strict ladder: Management API, stable machine-readable endpoint, structured file, then a closed direct-argv subprocess mapping. PMux parses no human-readable upstream output except the isolated startup banner used for last-resort version detection. Endpoint capability is probed before use; version alone never proves a feature exists. Application and presentation packages depend on typed domain ports rather than transport details.

## Consequences

Fallbacks are explicit and disclosed before use. All route knowledge stays in the management adapter, all core process execution stays in the subprocess adapter, and provider identifiers can never be interpolated into command-line flags. Features without a safe machine-readable surface remain unavailable rather than relying on prose parsing.
