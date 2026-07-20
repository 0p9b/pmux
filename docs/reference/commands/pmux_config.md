## pmux config

Inspect or change configuration

```
pmux config [flags]
```

### Options

```
  -h, --help           help for config
      --scope string   configuration scope: proxy or pmux (default "proxy")
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
* [pmux config backup](pmux_config_backup.md)	 - Create a private checksummed backup
* [pmux config edit](pmux_config_edit.md)	 - Edit one private temporary configuration copy
* [pmux config get](pmux_config_get.md)	 - Get one redacted value
* [pmux config restore](pmux_config_restore.md)	 - Restore an exact validated backup
* [pmux config set](pmux_config_set.md)	 - Set one known configuration value
* [pmux config show](pmux_config_show.md)	 - Show redacted configuration

