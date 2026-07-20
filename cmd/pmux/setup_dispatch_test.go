package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/app"
)

func TestInteractiveSetupWithOmittedModeUsesGuidedChoice(t *testing.T) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	spy := &commandSpy{}
	deps := testDependencies(spy, true, out, stderr)
	called := false
	deps.RunSetupTUI = func(_ context.Context, _ dependencies, _ *globalFlags, start setupTUIOptions) error {
		called = true
		if start.Mode != "" {
			t.Fatalf("omitted mode became %q before terminal choice", start.Mode)
		}
		return nil
	}
	code := execute(context.Background(), deps, []string{"setup"})
	if code != 0 || !called {
		t.Fatalf("exit=%d called=%v stderr=%s", code, called, stderr)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("dispatcher bypassed guided choice: %#v", spy.calls)
	}
}

func TestInteractiveExplicitManagedSetupStillUsesGuidedContinuation(t *testing.T) {
	deps := testDependencies(&commandSpy{}, true, &bytes.Buffer{}, &bytes.Buffer{})
	var got setupTUIOptions
	deps.RunSetupTUI = func(_ context.Context, _ dependencies, _ *globalFlags, start setupTUIOptions) error {
		got = start
		return nil
	}
	code := execute(context.Background(), deps, []string{"setup", "--mode", "managed"})
	if code != 0 || got.Mode != "managed" {
		t.Fatalf("exit=%d setup options=%#v", code, got)
	}
}

func TestSetupOmittedModeRejectedOutsideInteractiveTerminal(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal bool
		args     []string
	}{
		{name: "non-tty", args: []string{"setup"}},
		{name: "json", terminal: true, args: []string{"--json", "setup"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			spy := &commandSpy{}
			deps := testDependencies(spy, test.terminal, out, stderr)
			deps.RunSetupTUI = func(context.Context, dependencies, *globalFlags, setupTUIOptions) error {
				t.Fatal("noninteractive setup opened terminal choice")
				return nil
			}
			code := execute(context.Background(), deps, test.args)
			if code != 2 {
				t.Fatalf("exit=%d output=%s stderr=%s", code, out, stderr)
			}
			combined := out.String() + stderr.String()
			if !strings.Contains(combined, "requires --mode") {
				t.Fatalf("missing mode error: %s", combined)
			}
			if len(spy.calls) != 0 {
				t.Fatalf("invalid setup reached app: %#v", spy.calls)
			}
		})
	}
}

var _ app.UseCases = (*commandSpy)(nil)
