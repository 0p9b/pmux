# ADR-001: Build PMux as a standalone companion

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

PMux must install or adopt CLIProxyAPI, manage native lifecycle, authenticate providers, discover models, launch coding clients, and diagnose failures from a terminal. CLIProxyAPI plugins are in-process CGO libraries and share the proxy crash domain. A fork would make PMux responsible for protocol translation, routing, and credential internals. The upstream TUI is focused on managing one running proxy and does not own installation, native services, client launch, or repair.

## Decision

PMux is an independent Go executable and a peer companion to CLIProxyAPI. It does not become a plugin, fork CLIProxyAPI, or depend on an eventual upstream-TUI contribution. CLIProxyAPI remains the engine for proxying, credential refresh, protocol translation, and routing.

## Consequences

PMux has a separate release lifecycle and communicates through explicit integration ports. It must maintain compatibility probes and subprocess boundaries, but it avoids the core's crash domain and preserves a single self-contained terminal binary on every v1 target.
