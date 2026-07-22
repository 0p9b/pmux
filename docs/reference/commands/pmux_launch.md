## pmux launch

Launch a coding client

```
pmux launch --client <client> --model <id> [-- <client args>] [flags]
```

### Options

```
      --client string               coding client: claude, codex, gemini, or opencode
      --fallback strings            exact fallback model IDs tried in order (comma-separated or repeatable)
  -h, --help                        help for launch
      --model string                exact dynamically discovered model ID
      --profile pmux profiles set   named launch profile from pmux profiles set
```

### Options inherited from parent commands

```
      --config-dir string   override the PMux config root
      --json                emit machine-readable JSON or NDJSON
      --verbose             write safe diagnostic details to stderr
      --yes                 accept the command's defined confirmation
```

### SEE ALSO

* [pmux](pmux.md)	 - Terminal control plane for CLIProxyAPI

