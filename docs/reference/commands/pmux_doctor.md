## pmux doctor

Diagnose and repair PMux

```
pmux doctor [flags]
```

### Options

```
      --bundle string[="<default>"]   create a redacted bundle at an optional path
      --check strings                 run a named check (repeatable)
      --fix strings[=*]               apply named fixes, or all eligible fixes when omitted
  -h, --help                          help for doctor
      --online                        include explicit online checks
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

