//go:build darwin

package platform

import (
	"context"
	"io"
	"path/filepath"
	"strings"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type nativePlatform struct{ base }

func newNative(configOverride string) *nativePlatform {
	return &nativePlatform{base: newBase(configOverride)}
}

func (p *nativePlatform) GOOS() string { return "darwin" }

func (p *nativePlatform) nativeRoots() (config, state, cache, data string, err error) {
	home, err := p.home()
	if err != nil {
		return "", "", "", "", err
	}
	applicationSupport := rootsFromHome(home, "Library", "Application Support", "PMux")
	return applicationSupport, filepath.Join(applicationSupport, "State"), rootsFromHome(home, "Library", "Caches", "PMux"), applicationSupport, nil
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

func defaultServiceBackend(context.Context) service.ServiceBackend {
	return service.BackendLaunchd
}

func (p *nativePlatform) OpenBrowser(ctx context.Context, url string) error {
	return p.runHelper(ctx, []string{"open"}, url)
}

func (p *nativePlatform) SetClipboard(text string) error {
	path, err := p.lookPath("pbcopy")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.CodeDependencyMissing, pmuxerr.Environment, "the pbcopy clipboard helper was not found")
	}
	cmd := p.command(context.Background(), path)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return helperError(err, "could not run the pbcopy clipboard helper")
	}
	return nil
}

func (p *nativePlatform) Shell() string {
	if shell := safeEnvironmentValue(p.getenv("SHELL")); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func (p *nativePlatform) IsWSL() bool { return false }

func (p *nativePlatform) SecurePermissions(path string, isDir bool) error {
	return secureUnixPermissions(path, isDir)
}

func (p *nativePlatform) VerifySecurePermissions(path string, isDir bool) error {
	return verifyUnixPermissions(path, isDir)
}

