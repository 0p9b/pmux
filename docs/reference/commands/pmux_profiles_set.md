## pmux profiles set

Create or replace a launch profile

```
pmux profiles set <name> --client <client> --model <id> [flags]
```

### Options

```
      --arg stringArray    extra client argument (repeatable)
      --client string      coding client: claude, codex, gemini, or opencode
      --fallback strings   exact fallback model IDs tried in order
  -h, --help               help for set
      --model string       exact dynamically discovered model ID
```

### Options inherited from parent commands

```
      --config-dir string   override the PMux config root
      --json                emit machine-readable JSON or NDJSON
      --verbose             write safe diagnostic details to stderr
      --yes                 accept the command's defined confirmation
```

### SEE ALSO

* [pmux profiles](pmux_profiles.md)	 - Manage named launch profiles

