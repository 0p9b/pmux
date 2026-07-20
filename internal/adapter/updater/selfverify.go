package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

const maxVersionOutput = 4096

// CommandSelfVerifier invokes PMux's canonical version command by direct argv.
// It never uses a shell or exposes child output in an error.
type CommandSelfVerifier struct{}

func (CommandSelfVerifier) Preflight(ctx context.Context, candidate, expectedVersion string) error {
	return verifyCommandVersion(ctx, candidate, expectedVersion)
}

func (CommandSelfVerifier) Postflight(ctx context.Context, active, expectedVersion string) error {
	return verifyCommandVersion(ctx, active, expectedVersion)
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("version output exceeded safe limit")
	}
	return b.Buffer.Write(p)
}

func verifyCommandVersion(ctx context.Context, executable, expectedVersion string) error {
	cmd := exec.CommandContext(ctx, executable, "version", "--short")
	cmd.Env = []string{}
	stdout := &boundedBuffer{limit: maxVersionOutput}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return &pmuxerr.Error{Code: pmuxerr.InstallIntegrityFailed, Class: pmuxerr.Environment, Message: "PMux version verification failed.", Evidence: []string{fmt.Sprintf("exit: %v", err)}, Cause: err}
	}
	observed := strings.TrimSpace(stdout.String())
	expected := strings.TrimSpace(expectedVersion)
	if observed == "" || expected == "" || strings.TrimPrefix(observed, "v") != strings.TrimPrefix(expected, "v") {
		return &pmuxerr.Error{Code: pmuxerr.InstallIntegrityFailed, Class: pmuxerr.Upstream, Message: "PMux version verification returned an unexpected version.", Evidence: []string{"expected " + expected, "version output did not match"}}
	}
	return nil
}
