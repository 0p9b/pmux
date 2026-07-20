//go:build linux

package platform

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type nativePlatform struct {
	base
	readFile func(string) ([]byte, error)
}

func newNative(configOverride string) *nativePlatform {
	return &nativePlatform{base: newBase(configOverride), readFile: os.ReadFile}
}

func (p *nativePlatform) GOOS() string { return "linux" }

func (p *nativePlatform) nativeRoots() (config, state, cache, data string, err error) {
	home, err := p.home()
	if err != nil {
		return "", "", "", "", err
	}
	config = absoluteEnvRoot(p.getenv("XDG_CONFIG_HOME"))
	if config == "" {
		config = rootsFromHome(home, ".config")
	}
	state = absoluteEnvRoot(p.getenv("XDG_STATE_HOME"))
	if state == "" {
		state = rootsFromHome(home, ".local", "state")
	}
	cache = absoluteEnvRoot(p.getenv("XDG_CACHE_HOME"))
	if cache == "" {
		cache = rootsFromHome(home, ".cache")
	}
	data = absoluteEnvRoot(p.getenv("XDG_DATA_HOME"))
	if data == "" {
		data = rootsFromHome(home, ".local", "share")
	}
	return filepath.Join(config, "pmux"), filepath.Join(state, "pmux"), filepath.Join(cache, "pmux"), filepath.Join(data, "pmux"), nil
}

func (p *nativePlatform) ConfigDir() (string, error) {
	config, _, _, _, err := p.nativeRoots()
	if err != nil {
		return "", err
	}
	return p.configDir(config), nil
}

func (p *nativePlatform) StateDir() (string, error) {
	_, state, _, _, err := p.nativeRoots()
	return state, err
}

func (p *nativePlatform) CacheDir() (string, error) {
	_, _, cache, _, err := p.nativeRoots()
	return cache, err
}

func (p *nativePlatform) DataDir() (string, error) {
	_, _, _, data, err := p.nativeRoots()
	return data, err
}

func defaultServiceBackend(ctx context.Context) service.ServiceBackend {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return service.BackendForeground
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return service.BackendForeground
	}
	command := exec.CommandContext(ctx, systemctl, "--user", "show-environment")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return service.BackendForeground
	}
	return service.BackendSystemdUser
}

func (p *nativePlatform) OpenBrowser(ctx context.Context, url string) error {
	if p.IsWSL() {
		return p.runHelper(ctx, []string{"wslview", "explorer.exe"}, url)
	}
	return p.runHelper(ctx, []string{"xdg-open", "sensible-browser"}, url)
}

func (p *nativePlatform) SetClipboard(text string) error {
	candidates := []struct {
		name string
		args []string
	}{
		{name: "wl-copy"},
		{name: "xclip", args: []string{"-selection", "clipboard"}},
		{name: "xsel", args: []string{"--clipboard", "--input"}},
	}
	if p.IsWSL() {
		candidates = append([]struct {
			name string
			args []string
		}{{name: "clip.exe"}}, candidates...)
	}
	for _, candidate := range candidates {
		path, err := p.lookPath(candidate.name)
		if err != nil {
			continue
		}
		cmd := p.command(context.Background(), path, candidate.args...)
		cmd.Stdin = strings.NewReader(text)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return helperError(err, fmt.Sprintf("could not run the %s clipboard helper", candidate.name))
		}
		return nil
	}
	return pmuxerr.New(pmuxerr.CodeDependencyMissing, pmuxerr.Environment, "no supported clipboard helper was found")
}

func (p *nativePlatform) Shell() string {
	if shell := safeEnvironmentValue(p.getenv("SHELL")); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func (p *nativePlatform) IsWSL() bool {
	if p.getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	for _, path := range []string{"/proc/sys/kernel/osrelease", "/proc/version"} {
		contents, err := p.readFile(path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(contents))
		if strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl") {
			return true
		}
	}
	return false
}

func (p *nativePlatform) SecurePermissions(path string, isDir bool) error {
	return secureUnixPermissions(path, isDir)
}

func (p *nativePlatform) VerifySecurePermissions(path string, isDir bool) error {
	return verifyUnixPermissions(path, isDir)
}

