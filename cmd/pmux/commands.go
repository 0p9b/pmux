package main

import (
	"errors"
	"strings"

	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/spf13/cobra"
)

type dispatcher struct {
	deps  dependencies
	flags *globalFlags
}

type commandResultExit struct {
	code int
}

func (e *commandResultExit) Error() string {
	return "command completed with findings"
}

func (d dispatcher) interactive() bool {
	return !d.flags.JSON && d.deps.IsTerminal()
}

func (d dispatcher) run(cmd *cobra.Command, operation app.Operation, arguments []string, options map[string]any) error {
	if shouldRunTUI(operation, options) && d.interactive() && d.deps.RunTUI != nil {
		request := tuiRequest{Operation: operation, Arguments: append([]string(nil), arguments...), Options: cloneTUIOptions(options)}
		if err := d.deps.RunTUI(cmd.Context(), d.deps, d.flags, request); err != nil {
			var typed *pmuxerr.Error
			if errors.As(err, &typed) {
				return err
			}
			return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "PMux could not run the terminal interface.")
		}
		return nil
	}
	invocation := app.Invocation{
		Operation: operation, Arguments: append([]string(nil), arguments...),
		Options: options, ConfigDir: d.flags.ConfigDir, JSON: d.flags.JSON,
		Verbose: d.flags.Verbose, Yes: d.flags.Yes, Interactive: d.interactive(),
	}
	result, err := d.deps.UseCases.Execute(cmd.Context(), invocation, func(event app.Event) error {
		return renderEvent(d.deps.Out, event, d.flags.JSON)
	})
	if err != nil {
		var typed *pmuxerr.Error
		if !errors.As(err, &typed) {
			return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "PMux application services failed unexpectedly.")
		}
		return err
	}
	if !result.Streamed {
		if err := renderResult(d.deps.Out, result, d.flags.JSON); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "PMux could not render the command result.")
		}
	}
	if result.Attachment != nil {
		if err := result.Attachment(cmd.Context()); err != nil {
			var typed *pmuxerr.Error
			if errors.As(err, &typed) {
				return err
			}
			return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Upstream, "Foreground CLIProxyAPI exited.")
		}
	}
	if result.ExitCode != 0 {
		return &commandResultExit{code: result.ExitCode}
	}
	return nil
}

func shouldRunTUI(operation app.Operation, options map[string]any) bool {
	if isTUIOperation(operation) {
		return true
	}
	if operation != app.OpLaunch {
		return false
	}
	client, _ := options["client"].(string)
	model, _ := options["model"].(string)
	if client != "" && client != "claude" {
		return false
	}
	return client == "" || model == ""
}

func cloneTUIOptions(options map[string]any) map[string]any {
	if options == nil {
		return nil
	}
	cloned := make(map[string]any, len(options))
	for key, value := range options {
		cloned[key] = value
	}
	return cloned
}

func addCommands(root *cobra.Command, d dispatcher) {
	root.AddCommand(
		setupCommand(d), providersCommand(d), modelsCommand(d), launchCommand(d),
		claudeCommand(d), doctorCommand(d), serviceCommand(d), configCommand(d),
		updateCommand(d), completionCommand(d), versionCommand(d),
	)
}

type setupTUIOptions struct {
	Mode       string
	ProxyPath  string
	ConfigPath string
	Harden     bool
}

func setupCommand(d dispatcher) *cobra.Command {
	var mode, proxyPath, configPath string
	var harden bool
	cmd := &cobra.Command{Use: "setup", Short: "Set up or adopt CLIProxyAPI", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&mode, "mode", "", "setup mode: managed or adopt")
	cmd.Flags().StringVar(&proxyPath, "proxy-path", "", "absolute CLIProxyAPI executable path")
	cmd.Flags().StringVar(&configPath, "config-path", "", "absolute CLIProxyAPI config path")
	cmd.Flags().BoolVar(&harden, "harden", false, "run the separately previewed adoption hardening transaction")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if mode != "" && mode != "managed" && mode != "adopt" {
			return usage("--mode must be managed or adopt")
		}
		if !d.interactive() && mode == "" {
			return usage("Noninteractive operation requires --mode; no changes were made.")
		}
		if !d.interactive() && mode == "adopt" && proxyPath == "" {
			return usage("Noninteractive adoption requires --proxy-path; no changes were made.")
		}
		if !d.interactive() && harden && !d.flags.Yes {
			return usage("Noninteractive hardening requires --harden --yes; no changes were made.")
		}
		if d.interactive() && d.deps.RunSetupTUI != nil {
			return d.deps.RunSetupTUI(cmd.Context(), d.deps, d.flags, setupTUIOptions{
				Mode: mode, ProxyPath: proxyPath, ConfigPath: configPath, Harden: harden,
			})
		}
		return d.run(cmd, app.OpSetup, nil, opts("mode", mode, "proxy_path", proxyPath, "config_path", configPath, "harden", harden))
	}
	return cmd
}

func providersCommand(d dispatcher) *cobra.Command {
	parent := &cobra.Command{Use: "providers", Short: "Manage providers", Args: cobra.NoArgs}
	parent.RunE = func(cmd *cobra.Command, _ []string) error {
		op := app.OpProvidersList
		if d.interactive() {
			op = app.OpTUIProviders
		}
		if d.interactive() {
			return d.run(cmd, op, nil, nil)
		}
		return d.run(cmd, op, nil, opts("status", "", "type", "", "enabled", "", "refresh", false))
	}
	parent.AddCommand(providerListCommand(d), providerLoginCommand(d), providerVerifyCommand(d),
		providerToggleCommand(d, "enable"), providerToggleCommand(d, "disable"), providerRemoveCommand(d))
	return parent
}

func providerListCommand(d dispatcher) *cobra.Command {
	var status, typ, enabled string
	var refresh bool
	cmd := &cobra.Command{Use: "list", Short: "List providers", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&status, "status", "", "filter by status")
	cmd.Flags().StringVar(&typ, "type", "", "filter by capability type")
	cmd.Flags().StringVar(&enabled, "enabled", "", "filter by enabled state: true or false")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "refresh provider state")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if enabled != "" && enabled != "true" && enabled != "false" {
			return usage("--enabled must be true or false")
		}
		return d.run(cmd, "providers.list", nil, opts("status", status, "type", typ, "enabled", enabled, "refresh", refresh))
	}
	return cmd
}

func providerLoginCommand(d dispatcher) *cobra.Command {
	var method, apiKeyFile, serviceAccount, prefix string
	var noBrowser, callbackStdin, apiKeyStdin bool
	cmd := &cobra.Command{Use: "login <provider>", Short: "Authenticate or configure a provider", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&method, "method", "auto", "authentication method: auto, browser, or device")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open a browser")
	cmd.Flags().BoolVar(&callbackStdin, "callback-url-stdin", false, "read a callback URL from standard input")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "", "read an API key from a private file")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "read an API key from standard input")
	cmd.Flags().StringVar(&serviceAccount, "service-account", "", "private Vertex service-account JSON path")
	cmd.Flags().StringVar(&prefix, "vertex-prefix", "", "explicit Vertex import prefix")
	cmd.MarkFlagsMutuallyExclusive("api-key-file", "api-key-stdin")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if method != "auto" && method != "browser" && method != "device" {
			return usage("--method must be auto, browser, or device")
		}
		return d.run(cmd, "providers.login", args, opts("method", method, "no_browser", noBrowser, "callback_url_stdin", callbackStdin, "api_key_file", apiKeyFile, "api_key_stdin", apiKeyStdin, "service_account", serviceAccount, "vertex_prefix", prefix))
	}
	return cmd
}

func providerVerifyCommand(d dispatcher) *cobra.Command {
	var account string
	var refresh bool
	cmd := &cobra.Command{Use: "verify [provider]", Short: "Verify provider credentials", Args: cobra.MaximumNArgs(1)}
	cmd.Flags().StringVar(&account, "account", "", "verify one account label")
	cmd.Flags().BoolVar(&refresh, "refresh-models", false, "refresh models after verification")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return d.run(cmd, "providers.verify", args, opts("account", account, "refresh_models", refresh))
	}
	return cmd
}

func providerToggleCommand(d dispatcher, action string) *cobra.Command {
	cmd := &cobra.Command{Use: action + " <provider> [account]", Short: action + " a provider or account", Args: cobra.RangeArgs(1, 2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return d.run(cmd, app.Operation("providers."+action), args, nil)
	}
	return cmd
}

func providerRemoveCommand(d dispatcher) *cobra.Command {
	var keep bool
	cmd := &cobra.Command{Use: "remove <provider> [account]", Short: "Remove provider configuration or one account", Args: cobra.RangeArgs(1, 2)}
	cmd.Flags().BoolVar(&keep, "keep-credentials", false, "keep credential files")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return d.run(cmd, "providers.remove", args, opts("keep_credentials", keep))
	}
	return cmd
}

func modelsCommand(d dispatcher) *cobra.Command {
	parent := &cobra.Command{Use: "models", Short: "Discover and test models", Args: cobra.NoArgs}
	parent.RunE = func(cmd *cobra.Command, _ []string) error {
		op := app.OpModelsList
		if d.interactive() {
			op = app.OpTUIModels
		}
		if d.interactive() {
			return d.run(cmd, op, nil, nil)
		}
		return d.run(cmd, op, nil, opts("provider", "", "available", false, "favorite", false, "search", "", "refresh", false))
	}
	parent.AddCommand(modelListCommand(d), modelTestCommand(d), modelFavoriteCommand(d, "favorite"), modelFavoriteCommand(d, "unfavorite"))
	return parent
}

func modelListCommand(d dispatcher) *cobra.Command {
	var provider, search string
	var available, favorite, refresh bool
	cmd := &cobra.Command{Use: "list", Short: "List dynamically discovered models", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&provider, "provider", "", "filter by provider")
	cmd.Flags().BoolVar(&available, "available", false, "show available models only")
	cmd.Flags().BoolVar(&favorite, "favorite", false, "show favorites only")
	cmd.Flags().StringVar(&search, "search", "", "search model IDs and attribution")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "require a live refresh")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return d.run(cmd, "models.list", nil, opts("provider", provider, "available", available, "favorite", favorite, "search", search, "refresh", refresh))
	}
	return cmd
}

func modelTestCommand(d dispatcher) *cobra.Command {
	var timeout, provider string
	cmd := &cobra.Command{Use: "test <model>", Short: "Send one minimal model test", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&timeout, "timeout", "30s", "test timeout")
	cmd.Flags().StringVar(&provider, "provider", "", "disambiguate verified attribution")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return d.run(cmd, "models.test", args, opts("timeout", timeout, "provider", provider))
	}
	return cmd
}

func modelFavoriteCommand(d dispatcher, action string) *cobra.Command {
	cmd := &cobra.Command{Use: action + " <model>", Short: action + " an exact model ID", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return d.run(cmd, app.Operation("models."+action), args, nil)
	}
	return cmd
}

func launchCommand(d dispatcher) *cobra.Command {
	var client, model string
	cmd := &cobra.Command{Use: "launch --client claude --model <id> [-- <client args>]", Short: "Launch a coding client", Args: cobra.ArbitraryArgs}
	cmd.Flags().StringVar(&client, "client", "", "coding client (MVP: claude)")
	cmd.Flags().StringVar(&model, "model", "", "exact dynamically discovered model ID")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if client != "" && client != "claude" {
			return usage("--client must be claude")
		}
		if !d.interactive() && client == "" {
			return usage("Noninteractive launch requires --client; no client was started.")
		}
		if !d.interactive() && model == "" {
			return usage("Noninteractive launch requires --model; no client was started.")
		}
		if len(args) > 0 && cmd.ArgsLenAtDash() < 0 {
			return usage("client arguments must follow --")
		}
		if hasClientModelArg(args) {
			return usage("Client arguments must not contain '--model'; use 'pmux launch --model <id>'.")
		}
		return d.run(cmd, "launch", args, opts("client", client, "model", model))
	}
	return cmd
}

func claudeCommand(d dispatcher) *cobra.Command {
	cmd := &cobra.Command{Use: "claude <id> [-- <claude args>]", Short: "Launch Claude Code with an exact model", Args: cobra.ArbitraryArgs}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		dash := cmd.ArgsLenAtDash()
		if len(args) == 0 {
			return usage("pmux claude requires an exact model ID")
		}
		if dash < 0 && len(args) != 1 {
			return usage("Claude arguments must follow --")
		}
		if dash >= 0 && dash != 1 {
			return usage("pmux claude requires exactly one model ID before --")
		}
		if hasClientModelArg(args[1:]) {
			return usage("Client arguments must not contain '--model'; use 'pmux launch --model <id>'.")
		}
		return d.run(cmd, "launch", args[1:], opts("client", "claude", "model", args[0]))
	}
	return cmd
}

func doctorCommand(d dispatcher) *cobra.Command {
	var checks, fixes []string
	var bundle string
	var online bool
	cmd := &cobra.Command{Use: "doctor", Short: "Diagnose and repair PMux", Args: cobra.ArbitraryArgs}
	cmd.Flags().StringSliceVar(&checks, "check", nil, "run a named check (repeatable)")
	cmd.Flags().StringSliceVar(&fixes, "fix", nil, "apply named fixes, or all eligible fixes when omitted")
	cmd.Flags().Lookup("fix").NoOptDefVal = "*"
	cmd.Flags().StringVar(&bundle, "bundle", "", "create a redacted bundle at an optional path")
	cmd.Flags().Lookup("bundle").NoOptDefVal = "<default>"
	cmd.Flags().BoolVar(&online, "online", false, "include explicit online checks")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			if !cmd.Flags().Changed("fix") {
				return usage("doctor accepts positional IDs only after --fix")
			}
			fixes = append(fixes, args...)
		}
		if cmd.Flags().Changed("fix") && !d.interactive() && !d.flags.Yes {
			return usage("Noninteractive operation requires --yes; no changes were made.")
		}
		return d.run(cmd, "doctor", nil, opts("checks", checks, "fixes", fixes, "fix", cmd.Flags().Changed("fix"), "bundle", bundle, "online", online))
	}
	return cmd
}

func serviceCommand(d dispatcher) *cobra.Command {
	parent := &cobra.Command{Use: "service", Short: "Manage CLIProxyAPI lifecycle", Args: cobra.NoArgs}
	parent.RunE = func(cmd *cobra.Command, _ []string) error {
		op := app.OpServiceStatus
		if d.interactive() {
			op = app.OpTUIService
		}
		return d.run(cmd, op, nil, nil)
	}
	parent.AddCommand(simpleCommand(d, "status", "service.status", cobra.NoArgs), serviceStartCommand(d), serviceTimedCommand(d, "stop"), serviceTimedCommand(d, "restart"), serviceInstallCommand(d), simpleCommand(d, "uninstall", "service.uninstall", cobra.NoArgs), serviceLogsCommand(d))
	return parent
}

func simpleCommand(d dispatcher, use string, operation app.Operation, validator cobra.PositionalArgs) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: use, Args: validator}
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return d.run(cmd, operation, args, nil) }
	return cmd
}

func serviceStartCommand(d dispatcher) *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{Use: "start", Short: "Start the recorded backend", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run attached in the foreground")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if d.flags.JSON && foreground {
			return usage("--json cannot be combined with service start --foreground; no process was started.")
		}
		return d.run(cmd, "service.start", nil, opts("foreground", foreground))
	}
	return cmd
}

func serviceTimedCommand(d dispatcher, action string) *cobra.Command {
	var timeout string
	cmd := &cobra.Command{Use: action, Short: action + " the recorded backend", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&timeout, "timeout", "", "graceful stop timeout")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return d.run(cmd, app.Operation("service."+action), nil, opts("timeout", timeout))
	}
	return cmd
}

func serviceInstallCommand(d dispatcher) *cobra.Command {
	var start bool
	cmd := &cobra.Command{Use: "install", Short: "Install the native service definition", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&start, "start", false, "start after installation")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return d.run(cmd, "service.install", nil, opts("start", start))
	}
	return cmd
}

func serviceLogsCommand(d dispatcher) *cobra.Command {
	var source, level, since, output, clear string
	var lines int
	var follow bool
	cmd := &cobra.Command{Use: "logs", Short: "Read redacted service logs", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&source, "source", "all", "pmux, proxy, service, request-error, or all")
	cmd.Flags().StringVar(&level, "level", "", "filter by log level")
	cmd.Flags().IntVar(&lines, "lines", 100, "maximum initial log lines")
	cmd.Flags().StringVar(&since, "since", "", "show logs since a time")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow new entries")
	cmd.Flags().StringVar(&output, "output", "", "write redacted logs to a private file")
	cmd.Flags().StringVar(&clear, "clear", "", "clear one supported upstream log source")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if lines < 0 {
			return usage("--lines must not be negative")
		}
		if clear != "" {
			for _, name := range []string{"source", "level", "lines", "since", "follow", "output"} {
				if cmd.Flags().Changed(name) {
					return usage("--clear cannot be combined with log read, filter, follow, or output flags")
				}
			}
		}
		return d.run(cmd, "service.logs", nil, opts("source", source, "level", level, "lines", lines, "since", since, "follow", follow, "output", output, "clear", clear))
	}
	return cmd
}

func configCommand(d dispatcher) *cobra.Command {
	var scope string
	parent := &cobra.Command{Use: "config", Short: "Inspect or change configuration", Args: cobra.NoArgs}
	parent.PersistentFlags().StringVar(&scope, "scope", "proxy", "configuration scope: proxy or pmux")
	validScope := func() error {
		if scope != "proxy" && scope != "pmux" {
			return usage("--scope must be proxy or pmux")
		}
		return nil
	}
	parent.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := validScope(); err != nil {
			return err
		}
		if d.interactive() {
			return d.run(cmd, app.OpTUIConfig, nil, opts("scope", scope))
		}
		return d.run(cmd, app.OpConfigShow, nil, opts("scope", scope, "effective", false, "reveal_paths", false))
	}
	parent.AddCommand(configShowCommand(d, &scope, validScope), configGetCommand(d, &scope, validScope), configSetCommand(d, &scope, validScope), configEditCommand(d, &scope, validScope), configBackupCommand(d, &scope, validScope), configRestoreCommand(d, &scope, validScope))
	return parent
}

func configShowCommand(d dispatcher, scope *string, valid func() error) *cobra.Command {
	var effective, reveal bool
	cmd := &cobra.Command{Use: "show", Short: "Show redacted configuration", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&effective, "effective", false, "include source and activation metadata")
	cmd.Flags().BoolVar(&reveal, "reveal-paths", false, "reveal paths, never secrets")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := valid(); err != nil {
			return err
		}
		return d.run(cmd, "config.show", nil, opts("scope", *scope, "effective", effective, "reveal_paths", reveal))
	}
	return cmd
}

func configGetCommand(d dispatcher, scope *string, valid func() error) *cobra.Command {
	cmd := &cobra.Command{Use: "get <key>", Short: "Get one redacted value", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := valid(); err != nil {
			return err
		}
		return d.run(cmd, "config.get", args, opts("scope", *scope))
	}
	return cmd
}

func configSetCommand(d dispatcher, scope *string, valid func() error) *cobra.Command {
	var restart bool
	cmd := &cobra.Command{Use: "set <key> <value>", Short: "Set one known configuration value", Args: cobra.ExactArgs(2)}
	cmd.Flags().BoolVar(&restart, "restart", false, "restart after a restart-required change")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := valid(); err != nil {
			return err
		}
		return d.run(cmd, "config.set", args, opts("scope", *scope, "restart", restart))
	}
	return cmd
}

func configEditCommand(d dispatcher, scope *string, valid func() error) *cobra.Command {
	var editor string
	var restart bool
	cmd := &cobra.Command{Use: "edit", Short: "Edit one private temporary configuration copy", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&editor, "editor", "", "editor executable")
	cmd.Flags().BoolVar(&restart, "restart", false, "restart after a restart-required change")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := valid(); err != nil {
			return err
		}
		if !d.interactive() {
			return usage("This operation requires an interactive terminal; use `pmux config set` instead.")
		}
		return d.run(cmd, "config.edit", nil, opts("scope", *scope, "editor", editor, "restart", restart))
	}
	return cmd
}

func configBackupCommand(d dispatcher, scope *string, valid func() error) *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Create a private checksummed backup", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := valid(); err != nil {
			return err
		}
		return d.run(cmd, "config.backup", nil, opts("scope", *scope))
	}
	return cmd
}

func configRestoreCommand(d dispatcher, scope *string, valid func() error) *cobra.Command {
	var restart bool
	cmd := &cobra.Command{Use: "restore <backup>", Short: "Restore an exact validated backup", Args: cobra.ExactArgs(1)}
	cmd.Flags().BoolVar(&restart, "restart", false, "restart after restore")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := valid(); err != nil {
			return err
		}
		return d.run(cmd, "config.restore", args, opts("scope", *scope, "restart", restart))
	}
	return cmd
}

func updateCommand(d dispatcher) *cobra.Command {
	parent := &cobra.Command{Use: "update", Short: "Check for or apply explicit updates", Args: cobra.NoArgs}
	parent.RunE = func(cmd *cobra.Command, _ []string) error {
		return d.run(cmd, "update.check", nil, opts("component", "all"))
	}
	parent.AddCommand(updateCheckCommand(d), updateComponentCommand(d, "self"), updateComponentCommand(d, "proxy"))
	return parent
}

func updateCheckCommand(d dispatcher) *cobra.Command {
	var component string
	cmd := &cobra.Command{Use: "check", Short: "Check release metadata without changing files", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&component, "component", "all", "component: all, self, or proxy")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if component != "all" && component != "self" && component != "proxy" {
			return usage("--component must be all, self, or proxy")
		}
		return d.run(cmd, "update.check", nil, opts("component", component))
	}
	return cmd
}

func updateComponentCommand(d dispatcher, component string) *cobra.Command {
	var target string
	cmd := &cobra.Command{Use: component, Short: "Explicitly update " + component, Args: cobra.NoArgs}
	cmd.Flags().StringVar(&target, "version", "", "exact semantic version")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return d.run(cmd, app.Operation("update."+component), nil, opts("version", target))
	}
	return cmd
}

func opts(values ...any) map[string]any {
	if len(values)%2 != 0 {
		panic("opts requires key/value pairs")
	}
	out := make(map[string]any, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		if key, ok := values[i].(string); ok {
			out[key] = values[i+1]
		}
	}
	return out
}

func hasClientModelArg(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--model" || strings.HasPrefix(argument, "--model=") {
			return true
		}
	}
	return false
}

func usage(message string) *pmuxerr.Error {
	err := pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, message)
	err.Explanation = "The command line does not match PMux's public grammar."
	err.Repair = []string{"Run `pmux --help` or the command's `--help` for the accepted form."}
	return err
}
