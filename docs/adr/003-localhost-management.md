# ADR-003: Keep management localhost-only

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

The Management API can change configuration, manage credentials, and make privileged requests. CLIProxyAPI rate-limits failed management authentication, but an authenticated remotely exposed management route would still grant installation-wide control. Phase-three fleet operations need remote administration without widening this boundary.

## Decision

PMux-managed and PMux-hardened configurations always set `remote-management.allow-remote: false`. PMux never offers a workflow that enables remote management and never sets `MANAGEMENT_PASSWORD`. Local operations use loopback. Future fleet operations reach the loopback Management API only through an authenticated SSH local forward using the user's SSH configuration and agent.

An intentionally non-loopback proxy bind does not alter this rule: proxy exposure and management locality are independent security boundaries.

## Consequences

Remote fleet work depends on SSH reachability and cannot directly call a LAN management port. Adopted installations with remote management enabled remain read-only until a separately previewed hardening transaction restores locality. Management credentials are never sent over a routable plain connection.
