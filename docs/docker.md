# Docker boundary

PMux v1 can detect, adopt read-only, and diagnose an existing CLIProxyAPI container. It does **not** create, start, stop, restart, replace, upgrade, remove, or package containers. The container runtime and the user's existing deployment remain the lifecycle owner.

This boundary is deliberate: native lifecycle support for Linux, macOS, Windows, and WSL is release-gating, while container packaging and managed lifecycle require separate mount, callback-port, update, ownership, and rollback acceptance.

## Read-only adoption

Use the normal adoption entry point with absolute host-visible paths when they can be resolved:

```sh
pmux setup --mode adopt \
  --proxy-path /absolute/path/to/cli-proxy-api \
  --config-path /absolute/path/to/config.yaml
```

PMux combines explicit paths, recorded container metadata, a reachable loopback endpoint, known image/mount evidence, and existing process/service facts. A documented `/CLIProxyAPI/config.yaml` mount or the official `eceasy/cli-proxy-api` image is a signal, not sufficient proof of ownership by itself.

The import transaction writes only PMux state. It does not execute a container command, change a mount, publish a port, rewrite config, rotate credentials, or alter restart policy.

## Available capabilities

When a local endpoint is reachable and management authentication is available, a Docker-backed adoption can use read-only status, provider/auth metadata, dynamic model listing, Management API logs, and non-mutating doctor checks. Structured host-visible config may be inspected under the same redaction rules.

```sh
pmux service status
pmux providers list
pmux models list --refresh
pmux doctor
pmux service logs --source proxy
```

Capability results depend on the reachable core and exposed management surfaces. PMux keeps management access on loopback and never recommends exposing it to a network to make Docker integration easier.

## Disabled lifecycle

For a Docker-backed instance, native service actions return an explicit message that lifecycle is owned by the container runtime. PMux does not silently switch the instance to foreground, systemd, launchd, or Task Scheduler. Proxy updates also remain with the owning container deployment.

Use the deployment's own reviewed Docker/Compose/orchestration procedure for lifecycle and updates. After an owner-initiated change, refresh PMux status and run:

```sh
pmux doctor
pmux models list --refresh
```

## Authentication callbacks

PMux does not modify container port publication. Prefer management paste-callback or device authorization where supported. If a provider flow requires a callback listener that the existing container does not publish, PMux diagnoses the mismatch and leaves remediation to the deployment owner rather than recreating the container.

## Later work

A PMux container image, Docker distribution, and PMux-managed container setup/lifecycle are later work. They are not part of phase 2's additional coding clients and are not implicitly included with phase 3 multi-host work.
