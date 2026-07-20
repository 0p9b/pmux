## pmux service logs

Read redacted service logs

```
pmux service logs [flags]
```

### Options

```
      --clear string    clear one supported upstream log source
      --follow          follow new entries
  -h, --help            help for logs
      --level string    filter by log level
      --lines int       maximum initial log lines (default 100)
      --output string   write redacted logs to a private file
      --since string    show logs since a time
      --source string   pmux, proxy, service, request-error, or all (default "all")
```

### Options inherited from parent commands

```
      --config-dir string   override the PMux config root
      --json                emit machine-readable JSON or NDJSON
      --verbose             write safe diagnostic details to stderr
      --yes                 accept the command's defined confirmation
```

### SEE ALSO

* [pmux service](pmux_service.md)	 - Manage CLIProxyAPI lifecycle

