package runtime

import (
	"context"
	"fmt"
	"os"

	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/charmbracelet/x/term"
)

// readPassword reads one protected value directly from the controlling input
// terminal. It never falls back to an echoed reader: callers must choose an
// explicit protected file/stdin command option for non-terminal input.
func (n *nativeRuntime) readPassword(ctx context.Context, prompt string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, ok := n.stdin.(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return nil, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Protected secret input requires an interactive terminal; use the command's protected file or stdin option instead.")
	}
	output := n.stderr
	if output == nil {
		output = os.Stderr
	}
	if prompt != "" {
		if _, err := fmt.Fprint(output, prompt); err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not render the protected input prompt")
		}
	}
	value, err := term.ReadPassword(input.Fd())
	_, newlineErr := fmt.Fprintln(output)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not read protected terminal input")
	}
	if newlineErr != nil {
		clear(value)
		return nil, pmuxerr.Wrap(newlineErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not finish the protected input prompt")
	}
	if err := ctx.Err(); err != nil {
		clear(value)
		return nil, err
	}
	return value, nil
}
