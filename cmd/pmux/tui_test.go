package main

import (
	"context"
	"testing"

	"github.com/0p9b/pmux/internal/app"
	pmuxtui "github.com/0p9b/pmux/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

func TestDispatcherRunsInjectedTUIForInteractiveOperation(t *testing.T) {
	t.Parallel()
	useCaseCalls := 0
	tuiCalls := 0
	deps := defaults(dependencies{
		UseCases: app.UseCaseFunc(func(context.Context, app.Invocation, app.EventSink) (app.Result, error) {
			useCaseCalls++
			return app.Result{}, nil
		}),
		IsTerminal: func() bool { return true },
		RunTUI: func(_ context.Context, _ dependencies, _ *globalFlags, request tuiRequest) error {
			tuiCalls++
			if request.Operation != app.OpTUIDashboard {
				t.Fatalf("operation = %q", request.Operation)
			}
			return nil
		},
	})
	d := dispatcher{deps: deps, flags: &globalFlags{}}
	if err := d.run(&cobra.Command{}, app.OpTUIDashboard, nil, nil); err != nil {
		t.Fatal(err)
	}
	if tuiCalls != 1 || useCaseCalls != 0 {
		t.Fatalf("TUI calls = %d, use-case calls = %d", tuiCalls, useCaseCalls)
	}
}

func TestInteractiveLaunchWithOmittedFieldsOpensSelectorAndPreservesInput(t *testing.T) {
	t.Parallel()
	var captured tuiRequest
	useCaseCalls := 0
	deps := defaults(dependencies{
		UseCases: app.UseCaseFunc(func(context.Context, app.Invocation, app.EventSink) (app.Result, error) {
			useCaseCalls++
			return app.Result{}, nil
		}),
		IsTerminal: func() bool { return true },
		RunTUI: func(_ context.Context, _ dependencies, _ *globalFlags, request tuiRequest) error {
			captured = request
			return nil
		},
	})
	arguments := []string{"--permission-mode", "plan", "argument with spaces"}
	options := map[string]any{"client": "claude", "model": ""}
	d := dispatcher{deps: deps, flags: &globalFlags{}}
	if err := d.run(&cobra.Command{}, app.OpLaunch, arguments, options); err != nil {
		t.Fatal(err)
	}
	if useCaseCalls != 0 {
		t.Fatal("interactive incomplete launch reached the router before selection")
	}
	if captured.Operation != app.OpLaunch || captured.Options["client"] != "claude" {
		t.Fatalf("selector request = %#v", captured)
	}
	if len(captured.Arguments) != len(arguments) {
		t.Fatalf("selector arguments = %#v, want %#v", captured.Arguments, arguments)
	}
	for i := range arguments {
		if captured.Arguments[i] != arguments[i] {
			t.Fatalf("selector arguments = %#v, want %#v", captured.Arguments, arguments)
		}
	}
	arguments[0] = "changed"
	options["client"] = "changed"
	if captured.Arguments[0] != "--permission-mode" || captured.Options["client"] != "claude" {
		t.Fatal("selector request retained caller-owned argument or option storage")
	}
}

func TestIncompleteLaunchStaysNoninteractiveForJSON(t *testing.T) {
	t.Parallel()
	tuiCalls := 0
	var got app.Invocation
	deps := defaults(dependencies{
		UseCases: app.UseCaseFunc(func(_ context.Context, invocation app.Invocation, _ app.EventSink) (app.Result, error) {
			got = invocation
			return app.Result{}, nil
		}),
		IsTerminal: func() bool { return true },
		RunTUI: func(context.Context, dependencies, *globalFlags, tuiRequest) error {
			tuiCalls++
			return nil
		},
	})
	d := dispatcher{deps: deps, flags: &globalFlags{JSON: true}}
	if err := d.run(&cobra.Command{}, app.OpLaunch, []string{"client-arg"}, map[string]any{"client": "", "model": ""}); err != nil {
		t.Fatal(err)
	}
	if tuiCalls != 0 {
		t.Fatal("JSON launch opened a TUI selector")
	}
	if got.Operation != app.OpLaunch || !got.JSON || got.Interactive {
		t.Fatalf("noninteractive invocation = %#v", got)
	}
}

func TestLaunchSelectorEligibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		options map[string]any
		want    bool
	}{
		{name: "both omitted", options: map[string]any{"client": "", "model": ""}, want: true},
		{name: "default Claude for exact model", options: map[string]any{"client": "", "model": "runtime-model"}, want: true},
		{name: "choose model", options: map[string]any{"client": "claude", "model": ""}, want: true},
		{name: "complete launch", options: map[string]any{"client": "claude", "model": "runtime-model"}, want: false},
		{name: "unsupported client stays an error", options: map[string]any{"client": "codex", "model": ""}, want: false},
	}
	for _, test := range cases {
		if got := shouldRunTUI(app.OpLaunch, test.options); got != test.want {
			t.Fatalf("%s: shouldRunTUI = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestTUIFacadeMapsDeferredLaunchModelAndExactClientArguments(t *testing.T) {
	t.Parallel()
	var got app.Invocation
	facade := &tuiFacade{useCases: app.UseCaseFunc(func(_ context.Context, invocation app.Invocation, _ app.EventSink) (app.Result, error) {
		got = invocation
		return app.Result{Data: map[string]any{"model": "runtime-model"}}, nil
	})}
	args := []string{"runtime-model", "--permission-mode", "plan", "argument with spaces"}
	if _, err := facade.Execute(context.Background(), pmuxtui.ActionRequest{ID: pmuxtui.ActionLaunchRun, Arguments: args}); err != nil {
		t.Fatal(err)
	}
	if got.Operation != app.OpLaunch || got.Options["model"] != "runtime-model" {
		t.Fatalf("invocation = %#v", got)
	}
	wantClientArgs := args[1:]
	if len(got.Arguments) != len(wantClientArgs) {
		t.Fatalf("client args = %#v, want %#v", got.Arguments, wantClientArgs)
	}
	for i := range wantClientArgs {
		if got.Arguments[i] != wantClientArgs[i] {
			t.Fatalf("client args = %#v, want %#v", got.Arguments, wantClientArgs)
		}
	}
}

func TestFinishShellTUIExecutesHandoffOnlyAfterProgramExit(t *testing.T) {
	t.Parallel()
	calls := 0
	var got app.Invocation
	snapshot := pmuxtui.Snapshot{
		Launch: pmuxtui.LaunchSnapshot{
			ModelID:   "runtime-model",
			Arguments: []string{"--permission-mode", "plan", "argument with spaces"},
		},
	}
	facade := &tuiFacade{
		useCases: app.UseCaseFunc(func(_ context.Context, invocation app.Invocation, _ app.EventSink) (app.Result, error) {
			calls++
			got = invocation
			return app.Result{}, nil
		}),
		snapshot: snapshot,
	}
	shell := pmuxtui.New(facade, snapshot, pmuxtui.Options{InitialScreen: pmuxtui.Launch, Plain: true})
	final, quit := shell.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if quit == nil {
		t.Fatal("launch did not request Program.Run exit")
	}
	_ = quit()
	if calls != 0 {
		t.Fatal("launch ran from the Bubble Tea command before terminal cleanup")
	}
	finalShell, ok := final.(*pmuxtui.Shell)
	if !ok {
		t.Fatalf("final model = %T, want *tui.Shell", final)
	}
	if err := finishShellTUI(context.Background(), facade, finalShell); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("post-run launch calls = %d, want 1", calls)
	}
	if got.Options["model"] != "runtime-model" {
		t.Fatalf("launch model = %#v", got.Options["model"])
	}
	want := snapshot.Launch.Arguments
	if len(got.Arguments) != len(want) {
		t.Fatalf("client args = %#v, want %#v", got.Arguments, want)
	}
	for i := range want {
		if got.Arguments[i] != want[i] {
			t.Fatalf("client args = %#v, want %#v", got.Arguments, want)
		}
	}
}

func TestRunTUIUsesRequestedInitialScreen(t *testing.T) {
	t.Parallel()
	if got := screenForOperation(app.OpTUIProviders); got != pmuxtui.Providers {
		t.Fatalf("screen = %v", got)
	}
	if got := screenForOperation(app.OpTUIModels); got != pmuxtui.Models {
		t.Fatalf("screen = %v", got)
	}
	if got := screenForOperation(app.OpTUIService); got != pmuxtui.Service {
		t.Fatalf("screen = %v", got)
	}
	if got := screenForOperation(app.OpTUIConfig); got != pmuxtui.Config {
		t.Fatalf("screen = %v", got)
	}
	if got := screenForRequest(tuiRequest{Operation: app.OpLaunch, Options: map[string]any{"client": "claude"}}); got != pmuxtui.Models {
		t.Fatalf("launch without model screen = %v, want Models", got)
	}
	if got := screenForRequest(tuiRequest{Operation: app.OpLaunch, Options: map[string]any{"model": "runtime-model"}}); got != pmuxtui.Launch {
		t.Fatalf("launch with model screen = %v, want Launch", got)
	}

}
