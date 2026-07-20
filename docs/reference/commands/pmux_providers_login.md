## pmux providers login

Authenticate or configure a provider

```
pmux providers login <provider> [flags]
```

### Options

```
      --api-key-file string      read an API key from a private file
      --api-key-stdin            read an API key from standard input
      --callback-url-stdin       read a callback URL from standard input
  -h, --help                     help for login
      --method string            authentication method: auto, browser, or device (default "auto")
      --no-browser               do not open a browser
      --service-account string   private Vertex service-account JSON path
      --vertex-prefix string     explicit Vertex import prefix
```

### Options inherited from parent commands

```
      --config-dir string   override the PMux config root
      --json                emit machine-readable JSON or NDJSON
      --verbose             write safe diagnostic details to stderr
      --yes                 accept the command's defined confirmation
```

### SEE ALSO

* [pmux providers](pmux_providers.md)	 - Manage providers

