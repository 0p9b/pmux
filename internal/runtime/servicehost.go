package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/0p9b/pmux/internal/adapter/service/foreground"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type ServiceHostStreams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// IsServiceHostInvocation identifies the private native-service process mode.
// It is intentionally absent from the public Cobra command tree.
func IsServiceHostInvocation(args []string) bool {
	return len(args) > 0 && args[0] == "--binary"
}

// RunServiceHost launches only the recorded CLIProxyAPI binary with an absolute
// -config argument. Native service definitions pass argv directly; no shell is
// involved and store-override environment variables are removed.
func RunServiceHost(ctx context.Context, args []string, streams ServiceHostStreams) error {
	values, err := parseServiceHostArgs(args)
	if err != nil {
		return err
	}
	binary, config := values["binary"], values["config"]
	for label, path := range map[string]string{"binary": binary, "config": config} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Service host "+label+" path must be absolute.")
		}
	}
	info, err := os.Stat(binary)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "CLIProxyAPI service binary is unavailable.")
	}
	if !info.Mode().IsRegular() || (goruntime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		return pmuxerr.New(pmuxerr.CodeNotExecutable, pmuxerr.Environment, "CLIProxyAPI service binary is not executable.")
	}
	runtimeDir := values["runtime-dir"]
	if runtimeDir != "" {
		if !filepath.IsAbs(runtimeDir) || filepath.Clean(runtimeDir) != runtimeDir {
			return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Service runtime path must be absolute.")
		}
		if _, err := os.Stat(filepath.Join(runtimeDir, ".env")); err == nil {
			return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "PMux service runtime directory contains a forbidden .env file.")
		} else if !errors.Is(err, os.ErrNotExist) {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not verify its service runtime directory.")
		}
	}
	stdout, stderr := streams.Stdout, streams.Stderr
	var logFile *os.File
	if logDir := values["log-dir"]; logDir != "" {
		if !filepath.IsAbs(logDir) || filepath.Clean(logDir) != logDir {
			return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Service log path must be absolute.")
		}
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not create its private service log directory.")
		}
		logFile, err = os.OpenFile(filepath.Join(logDir, "proxy.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not open its private proxy log.")
		}
		defer logFile.Close()
		stdout, stderr = logFile, logFile
	}
	cmd := exec.CommandContext(ctx, binary, "-config", config)
	cmd.Env = foreground.AllowlistedEnvironment(os.Environ())
	cmd.Dir = runtimeDir
	cmd.Stdin = streams.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Upstream, "CLIProxyAPI service process exited.")
	}
	return nil
}

func parseServiceHostArgs(args []string) (map[string]string, error) {
	allowed := map[string]bool{"--binary": true, "--config": true, "--runtime-dir": true, "--log-dir": true}
	values := make(map[string]string, len(allowed))
	for len(args) > 0 {
		flag := args[0]
		args = args[1:]
		if !allowed[flag] || len(args) == 0 || strings.HasPrefix(args[0], "--") {
			return nil, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Invalid private service-host invocation.")
		}
		if _, duplicate := values[strings.TrimPrefix(flag, "--")]; duplicate {
			return nil, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Duplicate private service-host argument.")
		}
		values[strings.TrimPrefix(flag, "--")] = args[0]
		args = args[1:]
	}
	if values["binary"] == "" || values["config"] == "" {
		return nil, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Private service host requires --binary and --config.")
	}
	return values, nil
}
