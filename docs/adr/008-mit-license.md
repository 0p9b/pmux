# ADR-008: License PMux under MIT

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

PMux is intended as a public companion in the CLIProxyAPI ecosystem. Contributors and package distributors need a simple, permissive license with clear rights to use, modify, redistribute, and package the program.

## Decision

The repository and PMux-authored source are licensed under the MIT License. The root `LICENSE` file is the authoritative license text. Contributions are accepted under the same license.

Third-party dependencies and redistributed upstream artifacts retain their own licenses; release and SBOM generation must not imply that MIT relicenses them.

## Consequences

Users may reuse and distribute PMux with minimal conditions, principally preserving the copyright and license notice. PMux provides no warranty. Dependency review and SPDX SBOM publication remain necessary to disclose the separate obligations of included dependencies.
