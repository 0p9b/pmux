//go:build !docsgen

package main

import (
	"context"
	"os"
	"os/signal"
	"strings"

	"github.com/0p9b/pmux/internal/adapter/updater"
	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/pmuxerr"
	pmuxruntime "github.com/0p9b/pmux/internal/runtime"
)

func main() {
	if updater.IsSelfUpdateHelperInvocation(os.Args[1:]) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := updater.RunSelfUpdateHelper(ctx, os.Args[1:]); err != nil {
			os.Exit(pmuxerr.ExitCode(err))
		}
		return
	}
	if pmuxruntime.IsServiceHostInvocation(os.Args[1:]) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		err := pmuxruntime.RunServiceHost(ctx, os.Args[1:], pmuxruntime.ServiceHostStreams{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
		if err != nil {
			_ = renderError(os.Stderr, err, false, false)
			os.Exit(pmuxerr.ExitCode(err))
		}
		return
	}
	useCases, err := pmuxruntime.NewNative(pmuxruntime.Options{ConfigDir: configDirArg(os.Args[1:]), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		useCases = app.UseCaseFunc(func(context.Context, app.Invocation, app.EventSink) (app.Result, error) {
			return app.Result{}, err
		})
	}
	deps := dependencies{
		UseCases:    useCases,
		RunTUI:      runTUI,
		RunSetupTUI: runSetupTUI,
		IsTerminal: func() bool {
			info, err := os.Stdin.Stat()
			return err == nil && info.Mode()&os.ModeCharDevice != 0
		},
	}
	os.Exit(execute(context.Background(), deps, os.Args[1:]))
}

func configDirArg(args []string) string {
	var value string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--config-dir=") {
			value = strings.TrimPrefix(arg, "--config-dir=")
			continue
		}
		if arg == "--config-dir" && index+1 < len(args) {
			index++
			value = args[index]
		}
	}
	return value
}
