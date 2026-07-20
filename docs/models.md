# Dynamic model discovery

PMux ships no model-ID catalog, provider-to-model constants, or guessed fallback defaults. Every selectable ID comes from the selected running CLIProxyAPI instance or a clearly marked cache of an earlier response from that same instance.

## Discovery ladder

A live refresh uses:

1. Management API auth-file metadata;
2. credential-associated `auth-files/models` responses;
3. `model-definitions/:channel` for channels actually observed;
4. authenticated `/v1/models` only when management attribution is unavailable or unusable.

The fallback `/v1/models` shape identifies vendor ownership, not the credential channel. PMux therefore renders provider attribution as `Unknown` rather than guessing from `owned_by` or a model ID.

```sh
pmux models list --refresh
pmux models list --provider codex --available
pmux models list --search kimi
pmux --json models list --refresh
```

Bare `pmux models` opens the Models TUI with a TTY and otherwise behaves as `pmux models list`.

Each record contains exact case-sensitive ID, vendor, verified channel/provider attribution when available, usable-account count, availability, favorite state, source/fetch time, and last test. Duplicate exact IDs merge management-verified channel and redacted credential references; PMux never rewrites an ID.

## Cache behavior

An interactive list may reuse a cache younger than 60 seconds. `--refresh` requires a live request. Successful provider login, removal, enable/disable, credential-config mutation, or observed auth-directory change invalidates the cache.

A failed refresh never replaces the last valid cache with a partial result. Offline data is display-only and visibly stale:

```text
Showing models cached <age> ago; live discovery failed: <safe reason>.
Launch is blocked until live availability can be verified.
```

A successful empty result is valid and exits successfully. PMux diagnoses the most specific observed cause: no credentials, safe mode, no usable credentials, or a compatibility limitation. It never fills an empty result with bundled IDs.

## Selection and favorites

Setup and Launch selectors present only live records. Even when one model is available, PMux asks the user to accept its exact ID; no universal default is preselected.

```sh
pmux models favorite <exact-id>
pmux models unfavorite <exact-id>
```

Favorites are PMux preferences, not Claude model slots. A favorite may remain recorded after it disappears, but it is unavailable until the exact ID reappears in a live catalog.

Unknown IDs fail with a refresh instruction:

```text
Model '<id>' is not in the current catalog; run "pmux models list --refresh".
```

## Minimal model test

```sh
pmux models test <exact-id>
pmux models test <exact-id> --timeout 30s --provider codex
```

PMux requires the ID in the live catalog and sends one minimal non-streaming request through the selected local proxy. It reports safe status and latency without generated content. Timeout, non-2xx, malformed response, or a safe-mode response fails the test. `--provider` only disambiguates verified attribution; it never rewrites the ID or selects an unverified credential.

Model tests are explicit network-capable actions and may consume provider quota. Provider verification alone does not run this completion test.

## JSON contract

A finite model-list response uses the standard `{"ok":true,"data":...}` envelope. Data identifies source, fetch time, staleness, warnings, and model records. Secrets and complete credential filenames are absent. An empty live list is a successful query. A required refresh network failure is a network error; a failed model test is unhealthy.

TUI and JSON records are projections of the same application result. Local TUI search never causes network activity; only explicit refresh or test actions do.
