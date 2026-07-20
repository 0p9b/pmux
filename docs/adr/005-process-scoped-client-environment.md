# ADR-005: Default to a process-scoped client environment

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

Claude Code v2 supports routing through `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`. Editing shell profiles or client settings during normal setup or launch would persist secrets, create conflicts, and make cleanup ambiguous. The selected runtime model must be exact and must not imply persistent opus, sonnet, or haiku assignments.

## Decision

Normal launch changes only the child process. PMux removes conflicting inherited Anthropic routing variables, adds only `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, and executes Claude Code with the exact discovered model as separate `--model` argv values. It never emits secret-bearing shell code or mutates the parent environment.

Persistent Claude settings are a separate explicit transaction with private backup, fingerprint conflict detection, redacted diff, confirmation, atomic write, verification, and an uninstall record. Opus, sonnet, and haiku are independently selected or unmanaged.

## Consequences

Normal launches are non-invasive and leave the shell and Claude settings unchanged. The proxy key remains visible to same-user process inspection during the child lifetime, an unavoidable limitation at PMux's privilege level. Persistent integration is available only with an auditable and reversible ownership boundary.
