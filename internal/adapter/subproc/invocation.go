package subproc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

// Invocation is a shell-free process invocation. Args excludes Path.
type Invocation struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// EnvironmentOptions controls the deliberately small child environment.
type EnvironmentOptions struct {
	GOOS          string
	OutboundProxy map[string]string
	TLS           map[string]string
}

var unixEnvironment = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "TMPDIR": {},
	"LANG": {}, "LC_ALL": {}, "TERM": {},
}

var windowsEnvironment = map[string]struct{}{
	"SYSTEMROOT": {}, "SYSTEMDRIVE": {}, "TEMP": {}, "TMP": {},
	"COMSPEC": {}, "PATHEXT": {},
}

var proxyEnvironment = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {},
}

var tlsEnvironment = map[string]struct{}{
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
}

// ScrubbedEnvironment constructs a complete allowlisted environment. In
// particular, it never propagates the store-selection, provider, management,
// or PMux variables from the parent.
func ScrubbedEnvironment(parent []string, opts EnvironmentOptions) []string {
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	allowed := make(map[string]struct{}, len(unixEnvironment)+len(windowsEnvironment))
	for key := range unixEnvironment {
		allowed[key] = struct{}{}
	}
	if goos == "windows" {
		for key := range windowsEnvironment {
			allowed[key] = struct{}{}
		}
	}

	values := make(map[string]string)
	for _, item := range parent {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.IndexByte(key, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			continue
		}
		upper := strings.ToUpper(key)
		if _, ok := allowed[upper]; ok {
			values[upper] = value
		}
	}
	for key, value := range opts.OutboundProxy {
		upper := strings.ToUpper(key)
		if _, ok := proxyEnvironment[upper]; ok && value != "" {
			values[upper] = value
		}
	}
	for key, value := range opts.TLS {
		upper := strings.ToUpper(key)
		if _, ok := tlsEnvironment[upper]; ok && value != "" {
			values[upper] = value
		}
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}


// LoginArgs returns the closed, source-verified CLIProxyAPI login mapping.
// The returned slice is independent and safe for the caller to append to.
func LoginArgs(id management.ProviderID, flow provider.AuthFlow, noBrowser bool) ([]string, error) {
	var args []string
	switch {
	case id == "codex" && flow == provider.FlowBrowser:
		args = []string{"-codex-login"}
	case id == "codex" && flow == provider.FlowDeviceCode:
		args = []string{"-codex-device-login"}
	case (id == "claude" || id == "anthropic") && flow == provider.FlowBrowser:
		args = []string{"-claude-login"}
	case id == "antigravity" && flow == provider.FlowBrowser:
		args = []string{"-antigravity-login"}
	case id == "kimi" && flow == provider.FlowDeviceCode:
		args = []string{"-kimi-login"}
	case id == "xai" && flow == provider.FlowDeviceCode:
		args = []string{"-xai-login"}
	default:
		return nil, pmuxerr.New(pmuxerr.AuthFileInvalid, pmuxerr.Upstream, fmt.Sprintf("provider %s has no supported CLIProxyAPI subprocess flow", id))
	}
	if noBrowser && (flow == provider.FlowBrowser) {
		args = append(args, "-no-browser")
	}
	return args, nil
}

// VertexImportArgs builds the only supported import subprocess argument map.
func VertexImportArgs(serviceAccountPath, prefix string) ([]string, error) {
	path, err := filepath.Abs(serviceAccountPath)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not resolve the Vertex service-account path")
	}
	if !filepath.IsAbs(path) {
		return nil, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.User, "Vertex service-account path must be absolute")
	}
	args := []string{"-vertex-import", path}
	if prefix != "" {
		args = append(args, "-vertex-import-prefix", prefix)
	}
	return args, nil
}

// BuildInvocation validates the executable, config, and runtime paths and
// appends an explicit absolute -config argument. It never invokes a shell.
func BuildInvocation(binaryPath, configPath, runtimeDir string, actionArgs, parentEnv []string, envOpts EnvironmentOptions) (Invocation, error) {
	binary, err := absolutePath(binaryPath, "CLIProxyAPI binary")
	if err != nil {
		return Invocation{}, err
	}
	config, err := absolutePath(configPath, "CLIProxyAPI config")
	if err != nil {
		return Invocation{}, err
	}
	dir, err := absolutePath(runtimeDir, "CLIProxyAPI runtime directory")
	if err != nil {
		return Invocation{}, err
	}
	if info, statErr := os.Stat(filepath.Join(dir, ".env")); statErr == nil && !info.IsDir() {
		return Invocation{}, pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "runtime directory contains .env; refusing to start CLIProxyAPI because CWD environment could override the recorded config")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return Invocation{}, pmuxerr.Wrap(statErr, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "could not verify the CLIProxyAPI runtime directory")
	}
	args := append([]string(nil), actionArgs...)
	args = append(args, "-config", config)
	return Invocation{Path: binary, Args: args, Env: ScrubbedEnvironment(parentEnv, envOpts), Dir: dir}, nil
}

func absolutePath(path, label string) (string, error) {
	if path == "" {
		return "", pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.User, label+" path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not resolve "+label+" path")
	}
	if !filepath.IsAbs(absolute) {
		return "", pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.User, label+" path must be absolute")
	}
	return filepath.Clean(absolute), nil
}
