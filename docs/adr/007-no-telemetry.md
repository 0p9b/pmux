# ADR-007: No telemetry, ever

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

PMux handles provider credentials, local topology, model choices, and diagnostic evidence. Background analytics, crash reporting, installation pings, and automatic update checks would violate predictable offline operation and create an unnecessary trust boundary.

## Decision

PMux contains no telemetry, analytics, crash-upload, metrics-export, or background update dependency or code path. Normal startup, cached views, service operations, configuration reads, read-only adoption, ordinary doctor, and diagnostic bundle creation make no non-loopback request. Network activity occurs only for an explicit install, provider authentication or verification, model test or refresh, `doctor --online`, update action, or launched client.

CI rejects known telemetry dependencies and suspicious telemetry imports outside `.tools`, and the release acceptance suite includes a deny-by-default zero-egress test.

## Consequences

Maintainers cannot add telemetry behind a preference or default-off switch; changing this decision requires superseding the product's privacy contract, not adding a setting. Operational debugging relies on local, redacted logs and user-created diagnostic bundles. Update freshness is user initiated.
