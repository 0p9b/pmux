package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestCommandSelfVerifierUsesCanonicalVersionCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture is a POSIX executable script")
	}
	executable := filepath.Join(t.TempDir(), "pmux")
	script := "#!/bin/sh\n[ \"$1\" = version ] && [ \"$2\" = --short ] || exit 12\nprintf 'v2.0.0\\n'\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	verifier := CommandSelfVerifier{}
	if err := verifier.Preflight(context.Background(), executable, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	err := verifier.Postflight(context.Background(), executable, "2.1.0")
	if err == nil {
		t.Fatal("version mismatch unexpectedly passed")
	}
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.InstallIntegrityFailed {
		t.Fatalf("unexpected mismatch error: %v", err)
	}
}
