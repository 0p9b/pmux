# Fleet management (phase 3 design)

> **Not available in v1.** This document records a future phase-3 boundary. The current pre-release binary must not register or advertise a fleet command, screen, daemon, or no-op placeholder.

Phase 3 will add bounded multi-host orchestration to the same PMux binary only after v1 single-host behavior and phase-2 client/profile contracts are stable. It will not introduce a PMux daemon or remote agent.

## Trust and discovery

The future implementation will read the user's `~/.ssh/config`, apply wildcard stanza defaults, and present concrete host aliases. It will follow include directives only within the defined support boundary, preserve host-key verification, and authenticate through the user's SSH agent. PMux will not request/store SSH passwords, generate/copy private keys, or edit SSH configuration.

Private provider credentials and auth files remain host-local. Each managed remote host receives independent proxy and management secrets; PMux never copies a credential between hosts.

## Read-only inventory first

Initial fleet behavior will be a read-only per-host matrix covering reachability, OS/architecture, core version or `unknown`, supported-floor status, service state, port, provider/auth metadata, doctor results, and models. One bounded-concurrency connection failure must not block results from other hosts.

Remote management traffic will traverse an SSH loopback forward. `remote-management.allow-remote` remains false. A host without management capability can expose only the read-only evidence available through its service, structured config, and safe metadata; PMux will not weaken locality to fill a matrix cell.

## Remote bootstrap

A later bootstrap transaction will:

1. probe the exact remote OS/architecture;
2. resolve that platform's canonical PMux roots rather than assuming Linux paths;
3. download the upstream asset on the controller, verify its release checksum before extraction, validate executable format/architecture, and transfer only verified content;
4. create the canonical version/current and per-instance data layout with private permissions or Windows DACLs;
5. generate unique host-local proxy and management secrets;
6. install the canonical service backend where supported;
7. verify `/healthz` and authenticated management through the SSH loopback forward.

It will not require a remote Go toolchain or execute an unverified installer. Operations will be journaled and resumable per host.

## Mutations and partial failure

Every future multi-host mutation will preview the exact resolved host set and per-host changes. Interactive use will approve/skip each host; noninteractive use will require explicit consent. Hosts are independent transactions. A successful host is not rolled back merely because another host was unreachable, and the final result will enumerate successes, failures, and safe retries without claiming global atomicity.

Concurrency will be bounded. A stop-on-error option may stop scheduling new work but cannot erase completed remote effects. Unknown port owners and foreign service definitions are evidence to display, never processes or files PMux kills/overwrites.

## Phase boundary

Phase 2 covers Codex CLI, Gemini CLI, OpenCode, named profiles, and fallback chains. Phase 3 covers the fleet work above. Docker packaging and PMux-managed container lifecycle remain separate later work; they are not silently bundled into either phase.

Until phase 3 ships, use one local PMux installation per host and each host's normal SSH/service tooling. Current documentation must not present a future fleet invocation as usable v1 syntax.
