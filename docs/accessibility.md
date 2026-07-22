# Accessibility

The supported accessibility baseline is complete CLI and machine-readable parity. The TUI is a convenience layer; terminal screen readers can work poorly with alternate-screen/full-redraw applications. Every operational TUI action must call the same application use case as a canonical CLI command and JSON or NDJSON representation. A TUI-only operational capability is release-blocking.

## CLI and JSON equivalents

| TUI capability | Canonical CLI | Machine form |
|---|---|---|
| Dashboard status/refresh | `pmux` | `pmux --json` |
| Managed setup | `pmux setup --mode managed` | add global `--json`; mutation requires global `--yes` |
| Read-only adoption | `pmux setup --mode adopt --proxy-path ... --config-path ...` | add global `--json` |
| Adoption hardening | same command plus `--harden` and global `--yes` | add global `--json` |
| Provider browse/auth/verify/manage | matching `pmux providers` subcommand | finite JSON or auth NDJSON |
| Model browse/test/favorite | matching `pmux models` subcommand | finite JSON |
| Coding client launch | `pmux launch --client <claude\|codex\|gemini\|opencode> --model <id>` or `pmux profiles` + `pmux launch --profile <name>` | use finite status/model/doctor JSON for automation; attached client owns terminal streams |
| Client API keys | `pmux keys list\|add\|remove` | finite JSON; a generated key is returned once in the add response |
| Model aliases/exclusions | `pmux models aliases ...`, `pmux models exclusions ...` | finite JSON |
| Quota reset | `pmux providers reset-quota <auth-file>` | finite JSON |
| Plugins | `pmux plugins list\|store\|install\|enable\|disable\|config\|remove` | finite JSON |
| Management panel | `pmux panel [--open]` | finite JSON; browser open is TTY-only |
| Doctor checks/fixes/bundle | `pmux doctor [--check ...] [--fix ...] [--bundle ...]` | finite JSON |
| Service lifecycle/status | matching `pmux service` subcommand | finite JSON |
| Log browse/follow/export/clear | `pmux service logs` flags | NDJSON for `--follow`, finite JSON otherwise |
| Proxy configuration | `pmux config --scope proxy ...` | finite JSON; editor remains TTY-only and each value has `set` |
| PMux settings | `pmux config --scope pmux ...` | finite JSON |
| Manual update | `pmux update check|self|proxy` | finite JSON |

Clipboard actions, focus movement, local search editing, pane layout, themes, columns, and optional vim keys are presentation interactions rather than separate operations.

## Machine-readable behavior

Put global flags before or after the command as accepted by Cobra; examples use them before the command for clarity:

```sh
pmux --json
pmux --json providers list --refresh
pmux --json models list --refresh
pmux --json doctor
pmux --json service logs --follow
```

Finite commands write one complete JSON object to stdout. Streaming authentication and log commands write one JSON object per line and exactly one terminal event for authentication. Log events include stable type, timestamp, instance ID, and redacted payload fields.

Global `--json` disables TUI, prompts, browser opening, clipboard writes, and editor launch. A mutation needing consent requires global `--yes`; missing consent or protected input fails without mutation. Human and machine forms use the same validation, locking, redaction, error, and exit behavior.

Complete secrets and protected callback URLs are absent from every machine representation. Device verification URI/user code may appear only in the transient active-auth event required for user action, not in persisted state, diagnostics, or terminal events.

## Keyboard behavior

The TUI is fully keyboard-operable:

- `1` through `9` jump to primary screens when no field/modal owns focus;
- `Tab` and `Shift+Tab` move focus in reading order;
- `/` opens local search on searchable lists;
- `Enter` activates or submits a valid field;
- `Esc` closes the current overlay, clears search, cancels an edit, or moves back one level; it does not quit from a primary screen;
- `q` quits only from a primary screen when no field/operation captures it;
- `?` shows context help and the exact CLI equivalent;
- `Ctrl+C` cancels a cancellable operation or quits when none is active.

Focus has a textual marker and non-color affordance. Selection and focus are distinct. Destructive confirmations name scope/consequence/backup and require an exact phrase; confirm remains disabled until it matches. Disabled controls stay visible with a reason.

## Color, motion, and reading order

Status always combines text and a marker, for example `Healthy`, `Warning`, `Error`, `Checking`, `Stopped`, or `Unknown`; color is supplementary. PMux honors `NO_COLOR`. High contrast is configured with:

```sh
pmux config --scope pmux set theme high-contrast
```

`TERM=dumb` selects line-oriented output. Animation always has a text status and can be disabled through PMux settings or `PMUX_NO_ANIMATION=1`. No required information exists only in a spinner, color, box art, or recording.

Headings, summary, evidence, actions, and help render in stable reading order. Every table row has a labeled linear detail view. Upstream text is control-character and terminal-escape sanitized before rendering.

## Terminal size and SSH

The supported interactive baseline is 80 columns by 24 rows. Below it, PMux shows only the current/required size and waits for resize or cancellation; it does not clip a confirmation or lose field values. Responsive layouts hide lower-priority columns and expose their content in the detail view.

In high-latency SSH mode, local focus/search never waits for network I/O. Refresh, authentication, model test, doctor-online checks, and log reconnect remain cancellable, show elapsed text status, retain last successful data, and avoid duplicate writes. Browser and clipboard success are never presumed; authorization URLs and device codes remain selectable plain text.

## Recordings

Project recordings have adjacent text transcripts containing every command, prompt, state transition, and result. Recordings use seeded redacted fixtures; no complete secret is present. Documentation and acceptance never require watching an animation or using the TUI when an equivalent CLI path exists.

## Report an accessibility defect

Use the [accessibility issue template](https://github.com/0p9b/pmux/issues/new/choose). Include the canonical CLI/JSON behavior, terminal and assistive technology, rendering mode, and the missing or divergent equivalent. Do not attach live secrets or unreviewed bundles.
