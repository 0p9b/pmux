# ADR-004: Preserve YAML comments and structure

- Status: Accepted
- Date: 2026-07-20
- Milestone: M0

## Context

Users hand-edit CLIProxyAPI YAML. Whole-file unmarshal and marshal can discard comments, mapping order, anchors, aliases, and unrelated formatting. PMux also needs semantic validation, concurrent-change detection, recoverable backups, and explicit restart classification.

## Decision

Domain configuration policy is expressed as semantic values and patch operations with no YAML dependency. Only `internal/adapter/configfile` may import `gopkg.in/yaml.v3`; it applies surgical AST edits and preserves untouched nodes. Direct writes require a fresh fingerprint, canonical private backup, same-directory temporary file, file and directory fsync, atomic replacement, read-back verification, and a semantic-diff check limited to requested paths.

Management API config writes use the same planning and validation policy but a distinct prior-GET backup, PUT, re-GET verification, and compensating PUT protocol; PMux does not claim upstream file-level atomicity.

## Consequences

Configuration edits are more deliberate than struct serialization and require AST-focused tests. Users retain comments and unrelated settings. Concurrent changes fail with an ownership conflict rather than being overwritten, and every committed mutation has a restorable exact-byte backup.
