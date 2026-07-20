package subproc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

var (
	bannerPattern  = regexp.MustCompile(`^CLIProxyAPI Version: (\S+), Commit: (\S+), BuiltAt: (.+)$`)
	ErrUnsafeProbe = pmuxerr.New(pmuxerr.InstallUnsupportedTarget, pmuxerr.Environment, "isolated version probe is unsafe")
)

// VersionInfo is the non-secret result of the isolated startup banner probe.
type VersionInfo struct {
	Version string
	Commit  string
	BuiltAt string
}

// VersionProbe starts only a generated isolated installation. It never accepts
// or reads the adopted user's config or auth directory.
type VersionProbe struct {
	Executor   LineExecutor
	TempRoot   string
	Timeout    time.Duration
	ParentEnv  []string
	EnvOptions EnvironmentOptions
}

func (p VersionProbe) Probe(ctx context.Context, binaryPath string) (VersionInfo, error) {
	binary, err := filepath.Abs(binaryPath)
	if err != nil || !filepath.IsAbs(binary) {
		return VersionInfo{}, ErrUnsafeProbe
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return VersionInfo{}, ErrUnsafeProbe
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return VersionInfo{}, ErrUnsafeProbe
	}
	if runtime.GOOS == "windows" {
		if err := validateProbeExecutable(binary); err != nil {
			return VersionInfo{}, ErrUnsafeProbe
		}
	}

	root, err := os.MkdirTemp(p.TempRoot, "pmux-version-probe-")
	if err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create an isolated version-probe directory")
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0o700); err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect the isolated version-probe directory")
	}
	authDir := filepath.Join(root, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create the isolated version-probe auth directory")
	}
	port, err := reserveLoopbackPort()
	if err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not reserve an isolated version-probe port")
	}
	key, err := temporaryProxyKey()
	if err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not generate an isolated version-probe key")
	}
	configPath := filepath.Join(root, "config.yaml")
	config := fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %q\napi-keys:\n  - %q\nremote-management:\n  allow-remote: false\n  disable-control-panel: true\nws-auth: true\n", port, authDir, key)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not write the isolated version-probe config")
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return VersionInfo{}, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect the isolated version-probe config")
	}

	invocation, err := BuildInvocation(binary, configPath, root, nil, p.ParentEnv, p.EnvOptions)
	if err != nil {
		return VersionInfo{}, err
	}
	executor := p.Executor
	if executor == nil {
		executor = ExecRunner{}
	}
	budget := p.Timeout
	if budget <= 0 {
		budget = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var result VersionInfo
	runErr := executor.Execute(probeCtx, invocation, func(line string) bool {
		match := bannerPattern.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			return true
		}
		result = VersionInfo{Version: match[1], Commit: match[2], BuiltAt: match[3]}
		return false
	})
	if result.Version != "" {
		return result, nil
	}
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return VersionInfo{}, ErrUnsafeProbe
		}
		return VersionInfo{}, runErr
	}
	return VersionInfo{}, ErrUnsafeProbe
}

func reserveLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func temporaryProxyKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(bytes), nil
}

func validateProbeExecutable(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return err
	}
	if magic[0] != 'M' || magic[1] != 'Z' {
		return errors.New("not a PE executable")
	}
	return nil
}
