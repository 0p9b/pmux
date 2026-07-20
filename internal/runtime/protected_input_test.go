package runtime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestReadPasswordRefusesEchoedNonTerminalInput(t *testing.T) {
	var output bytes.Buffer
	native := &nativeRuntime{stdin: strings.NewReader("secret\n"), stderr: &output}
	value, err := native.readPassword(context.Background(), "Key: ")
	if err == nil || value != nil {
		t.Fatalf("readPassword = %q, %v; want protected-input error", value, err)
	}
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeUsage {
		t.Fatalf("error = %#v; want typed usage error", err)
	}
	if output.Len() != 0 {
		t.Fatalf("non-terminal input rendered a prompt: %q", output.String())
	}
}

func TestReadPasswordHonorsCanceledContextBeforePrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	native := &nativeRuntime{stdin: strings.NewReader("secret\n"), stderr: &output}
	if _, err := native.readPassword(ctx, "Key: "); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if output.Len() != 0 {
		t.Fatalf("canceled input rendered a prompt: %q", output.String())
	}
}
