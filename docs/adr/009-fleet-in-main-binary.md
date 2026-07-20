# ADR-009: Keep fleet management in the main binary

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

Fleet management is phase-three scope. Introducing a controller daemon, remote agent, or second executable would add installation, update, authentication, and recovery surfaces before the local product is stable. Operators already have host identity and access policy in SSH configuration and agents.

## Decision

When phase three ships, fleet commands live in the `pmux` binary behind narrow domain ports. PMux reads the user's SSH configuration, authenticates through the user's SSH agent, preserves host-key checking, and reaches each remote loopback Management API through SSH forwarding. It deploys no daemon or remote PMux agent and copies no SSH private key or provider credential.

No `pmux fleet` command is registered before the feature is implemented and accepted.

## Consequences

One release artifact serves local and fleet workflows, and the existing SSH trust boundary remains authoritative. PMux inherits the reach of the user's SSH agent, so every mutation must preview the resolved target set, require confirmation, use bounded concurrency, and journal independent per-host results. Offline hosts fail independently rather than blocking completed hosts.
