package subproc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

// LineExecutor runs an invocation and delivers sanitized-by-caller process
// lines. Returning false from observe requests immediate process termination.
type LineExecutor interface {
	Execute(ctx context.Context, invocation Invocation, observe func(string) bool) error
}

// ExecRunner is the production shell-free LineExecutor.
type ExecRunner struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Sanitize func(string) string
}

func (r ExecRunner) Execute(ctx context.Context, invocation Invocation, observe func(string) bool) error {
	if err := ctx.Err(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "CLIProxyAPI subprocess was canceled before start")
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, invocation.Path, invocation.Args...)
	cmd.Dir = invocation.Dir
	cmd.Env = append([]string(nil), invocation.Env...)
	cmd.Stdin = r.Stdin
	prepareProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not capture CLIProxyAPI subprocess output")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not capture CLIProxyAPI subprocess errors")
	}
	if err := cmd.Start(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not start CLIProxyAPI subprocess")
	}

	lines := make(chan streamLine, 16)
	var readers sync.WaitGroup
	readers.Add(2)
	go scanStream(stdout, r.Stdout, r.Sanitize, lines, &readers)
	go scanStream(stderr, r.Stderr, r.Sanitize, lines, &readers)
	go func() {
		readers.Wait()
		close(lines)
	}()

	requestedStop := false
	var streamErr error
	for line := range lines {
		if line.err != nil {
			if streamErr == nil {
				streamErr = line.err
			}
			cancel()
			continue
		}
		if observe != nil && !observe(line.text) && !requestedStop {
			requestedStop = true
			cancel()
		}
	}
	waitErr := cmd.Wait()
	if streamErr != nil {
		return pmuxerr.Wrap(streamErr, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not read CLIProxyAPI subprocess output")
	}
	if requestedStop {
		return nil
	}
	if waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return pmuxerr.Wrap(ctxErr, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "CLIProxyAPI subprocess was canceled")
		}
		return pmuxerr.Wrap(waitErr, pmuxerr.ServiceStartFailed, pmuxerr.Upstream, "CLIProxyAPI subprocess failed")
	}
	return nil
}

type streamLine struct {
	text string
	err  error
}

func scanStream(input io.Reader, mirror io.Writer, sanitize func(string) string, lines chan<- streamLine, done *sync.WaitGroup) {
	defer done.Done()
	scanner := bufio.NewScanner(input)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		text := scanner.Text()
		if mirror != nil && sanitize != nil {
			_, _ = io.WriteString(mirror, sanitize(text)+"\n")
		}
		lines <- streamLine{text: text}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		lines <- streamLine{err: err}
	}
}

// AuthRunner is a configured subprocess fallback. It accepts the flags only so
// the application can journal its closed-map plan; it validates them against
// the domain registry before executing them and never derives a flag.
type AuthRunner struct {
	BinaryPath string
	ConfigPath string
	RuntimeDir string
	ParentEnv  []string
	EnvOptions EnvironmentOptions
	Executor   LineExecutor
	Observe    func(string) bool
	Sanitize   func(string) string
}

// RunAuth implements the provider-auth fallback handoff used by the
// application layer.
func (r *AuthRunner) RunAuth(ctx context.Context, providerID management.ProviderID, flow provider.AuthFlow, flags []string) error {
	expected, ok := registryFlags(providerID, flow)
	if !ok || !validAuthFlags(expected, flags, flow) {
		return pmuxerr.New(pmuxerr.AuthFileInvalid, pmuxerr.Upstream, "provider subprocess flags are not in the closed CLIProxyAPI mapping")
	}
	invocation, err := BuildInvocation(r.BinaryPath, r.ConfigPath, r.RuntimeDir, flags, r.ParentEnv, r.EnvOptions)
	if err != nil {
		return err
	}
	executor := r.Executor
	if executor == nil {
		executor = ExecRunner{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Sanitize: r.Sanitize}
	}
	return executor.Execute(ctx, invocation, r.Observe)
}

func registryFlags(id management.ProviderID, flow provider.AuthFlow) ([]string, bool) {
	for _, definition := range provider.Registry() {
		if definition.ID != id {
			continue
		}
		flags, ok := definition.SubprocessFlags[flow]
		return append([]string(nil), flags...), ok
	}
	// The Management API calls Claude "anthropic", but the core flag remains
	// the explicitly mapped Claude login flag.
	if id == "anthropic" && flow == provider.FlowBrowser {
		return []string{"-claude-login"}, true
	}
	return nil, false
}

func validAuthFlags(base, supplied []string, flow provider.AuthFlow) bool {
	if len(supplied) < len(base) || !sameStrings(base, supplied[:len(base)]) {
		return false
	}
	extra := supplied[len(base):]
	if len(extra) == 0 {
		return flow != provider.FlowVertexImport
	}
	if flow == provider.FlowBrowser {
		return len(extra) == 1 && extra[0] == "-no-browser"
	}
	if flow != provider.FlowVertexImport || len(extra) < 1 || !filepath.IsAbs(extra[0]) {
		return false
	}
	if len(extra) == 1 {
		return true
	}
	return len(extra) == 3 && extra[1] == "-vertex-import-prefix" && extra[2] != ""
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
