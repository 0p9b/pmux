# ADR-014: Use one foundation error package

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

Errors cross domain, adapter, application, CLI, TUI, JSON, and diagnostic boundaries. Independent adapter error shapes would lose stable condition identity, observed evidence, repair guidance, and wrapped causes, and would make secret redaction inconsistent.

## Decision

`internal/pmuxerr` defines the sole cross-layer error representation: stable PMUX condition code, responsibility class, safe message, explanation, observed evidence, ordered repair actions, documentation URL, and wrapped cause. Every layer creates or wraps this type; adapters do not expose a competing public taxonomy.

Specific conditions use the canonical condition registry, such as install-integrity failure, config safe mode, service-start failure, management unreachable, OAuth timeout, and missing client binary. Generic outcome codes remain command-boundary results only. Exit-code selection is centralized at the application/command boundary and is based on the concrete condition, not the PMUX numeric range. Causes are omitted from normal rendering and redacted before verbose rendering.

## Consequences

Human, TUI, and JSON output can project the same underlying failure without losing machine stability. Adding a condition requires a durable registry entry and mapping tests; removed codes remain tombstones and are never reused. Low-level causes remain available for safe diagnosis without leaking directly to users.
