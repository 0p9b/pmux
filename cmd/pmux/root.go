package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"os"
	"runtime"

	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/version"
	"github.com/spf13/cobra"
)

type dependencies struct {
	UseCases  app.UseCases
	RunTUI    func(context.Context, dependencies, *globalFlags, tuiRequest) error
	RunSetupTUI func(context.Context, dependencies, *globalFlags, setupTUIOptions) error
	In        io.Reader
	Out       io.Writer
	Err       io.Writer
	IsTerminal func() bool
	Version   func() version.Info
	GOOS      string
	GOARCH    string
	Getenv    func(string) string
	UserHome  func() (string, error)
}

type globalFlags struct {
	JSON      bool
	Verbose   bool
	Yes       bool
	ConfigDir string
}

func defaults(deps dependencies) dependencies {
	if deps.UseCases == nil {
		deps.UseCases = app.UnavailableUseCases{}
	}
	if deps.In == nil {
		deps.In = os.Stdin
	}
	if deps.Out == nil {
		deps.Out = os.Stdout
	}
	if deps.Err == nil {
		deps.Err = os.Stderr
	}
	if deps.IsTerminal == nil {
		deps.IsTerminal = func() bool { return false }
	}
	if deps.Version == nil {
		deps.Version = version.Current
	}
	if deps.GOOS == "" {
		deps.GOOS = runtime.GOOS
	}
	if deps.GOARCH == "" {
		deps.GOARCH = runtime.GOARCH
	}
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	if deps.UserHome == nil {
		deps.UserHome = os.UserHomeDir
	}
	return deps
}

// newRootCommand constructs a fresh command tree. No command state is shared
// between calls, which keeps parsing and injected-use-case tests deterministic.
func newRootCommand(raw dependencies) *cobra.Command {
	deps := defaults(raw)
	flags := &globalFlags{}
	root := &cobra.Command{
		Use:           "pmux",
		Short:         "Terminal control plane for CLIProxyAPI",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
	}
	root.SetIn(deps.In)
	root.SetOut(deps.Out)
	root.SetErr(deps.Err)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().BoolVar(&flags.JSON, "json", false, "emit machine-readable JSON or NDJSON")
	root.PersistentFlags().BoolVar(&flags.Verbose, "verbose", false, "write safe diagnostic details to stderr")
	root.PersistentFlags().BoolVar(&flags.Yes, "yes", false, "accept the command's defined confirmation")
	root.PersistentFlags().StringVar(&flags.ConfigDir, "config-dir", "", "override the PMux config root")

	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		if flags.ConfigDir == "" {
			return nil
		}
		absolute, err := filepath.Abs(flags.ConfigDir)
		if err != nil {
			wrapped := pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "PMux could not resolve --config-dir to an absolute path.")
			wrapped.Repair = []string{"Pass an accessible absolute path to --config-dir."}
			return wrapped
		}
		flags.ConfigDir = filepath.Clean(absolute)
		return nil
	}
	dispatch := dispatcher{deps: deps, flags: flags}
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		op := app.OpDashboardStatus
		if dispatch.interactive() {
			op = app.OpTUIDashboard
		}
		return dispatch.run(cmd, op, nil, nil)
	}
	addCommands(root, dispatch)
	return root
}

// execute is the process boundary: it owns error rendering and canonical exit
// mapping, while individual commands own success/event rendering.
func execute(ctx context.Context, raw dependencies, args []string) int {
	deps := defaults(raw)
	root := newRootCommand(deps)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var resultExit *commandResultExit
	if errors.As(err, &resultExit) {
		return resultExit.code
	}
	if ctx.Err() != nil {
		err = pmuxerr.Wrap(ctx.Err(), pmuxerr.CodeInterrupted, pmuxerr.User, "PMux was interrupted before command completion.")
	}
	err = commandError(err)
	jsonMode, _ := root.PersistentFlags().GetBool("json")
	verbose, _ := root.PersistentFlags().GetBool("verbose")
	target := deps.Err
	if jsonMode {
		target = deps.Out
	}
	_ = renderError(target, err, jsonMode, verbose)
	return exitCode(err)
}
