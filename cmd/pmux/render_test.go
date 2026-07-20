package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestVerboseCauseNeverRendersArbitraryErrorText(t *testing.T) {
	canaries := []string{
		"sk-proxy-canary-0123456789",
		"management-secret-canary-0123456789",
		"oauth-access-token-canary-0123456789",
		"-----BEGIN PRIVATE KEY-----private-key-canary",
	}
	cause := errors.New(strings.Join(canaries, " | "))
	typed := pmuxerr.Wrap(cause, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Management API is unavailable.")
	typed.Explanation = "The local management endpoint could not be reached."
	typed.Evidence = []string{"loopback request failed"}
	typed.Repair = []string{"Run `pmux doctor`."}

	for _, jsonMode := range []bool{false, true} {
		name := "human"
		if jsonMode { name = "json" }
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := renderError(&output, typed, jsonMode, true); err != nil { t.Fatal(err) }
			for _, canary := range canaries {
				if strings.Contains(output.String(), canary) { t.Fatalf("verbose output disclosed canary %q: %s", canary, output.String()) }
			}
			if !strings.Contains(output.String(), "*errors.errorString") { t.Fatalf("safe cause classification missing: %s", output.String()) }
			if !strings.Contains(output.String(), pmuxerr.ManagementUnreachable) { t.Fatalf("structural error code missing: %s", output.String()) }
		})
	}
}

func TestSafeCauseDoesNotInvokeErrorMethod(t *testing.T) {
	cause := &panicOnError{}
	if got := safeCause(cause); got != "*main.panicOnError" { t.Fatalf("classification = %q", got) }
}

type panicOnError struct{}

func (*panicOnError) Error() string { panic("safeCause called Error") }
