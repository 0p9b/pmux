package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	domainservice "github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

// New constructs the adapter for the current operating system. An optional
// config directory override affects ConfigDir only; the state, cache, and data
// roots always retain their platform defaults.
func New(configDirOverride ...string) (domainplatform.Platform, error) {
	if len(configDirOverride) > 1 {
		return nil, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "platform.New accepts at most one config directory override")
	}
	override := ""
	if len(configDirOverride) == 1 {
		var err error
		override, err = ResolveConfigOverride(configDirOverride[0])
		if err != nil {
			return nil, err
		}
	}
	return newNative(override), nil
}

// DefaultServiceBackend returns the native single-user lifecycle backend that
// is usable now. Linux selects systemd-user only when its user manager is
// reachable; otherwise foreground is the safe fallback.
func DefaultServiceBackend(ctx context.Context) domainservice.ServiceBackend {
	return defaultServiceBackend(ctx)
}

// ResolveConfigOverride resolves a CLI --config-dir value to a clean absolute
// path. An empty override remains empty so callers can select the native root.
func ResolveConfigOverride(override string) (string, error) {
	if override == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(override)
	if err != nil {
		return "", pathError(err, "could not resolve the PMux config directory override")
	}
	return filepath.Clean(absolute), nil
}

type base struct {
	configOverride string
	getenv         func(string) string
	homeDir        func() (string, error)
	lookPath       func(string) (string, error)
	command        func(context.Context, string, ...string) *exec.Cmd
}

func newBase(configOverride string) base {
	return base{
		configOverride: configOverride,
		getenv:         os.Getenv,
		homeDir:        os.UserHomeDir,
		lookPath:       exec.LookPath,
		command:        exec.CommandContext,
	}
}

func (b *base) configDir(native string) string {
	if b.configOverride != "" {
		return b.configOverride
	}
	return native
}

func (b *base) home() (string, error) {
	home, err := b.homeDir()
	if err != nil {
		return "", pathError(err, "could not determine the current user's home directory")
	}
	if home == "" {
		return "", pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "the current user's home directory is empty")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", pathError(err, "could not resolve the current user's home directory")
	}
	return filepath.Clean(absolute), nil
}

func (b *base) runHelper(ctx context.Context, candidates []string, args ...string) error {
	for _, candidate := range candidates {
		path, err := b.lookPath(candidate)
		if err != nil {
			continue
		}
		if err := b.command(ctx, path, args...).Run(); err != nil {
			return helperError(err, fmt.Sprintf("could not run the %s helper", candidate))
		}
		return nil
	}
	return pmuxerr.New(pmuxerr.CodeDependencyMissing, pmuxerr.Environment, "no supported platform helper was found")
}

func rootsFromHome(home string, parts ...string) string {
	return filepath.Join(append([]string{home}, parts...)...)
}

func absoluteEnvRoot(value string) string {
	if value == "" || !filepath.IsAbs(value) {
		return ""
	}
	return filepath.Clean(value)
}

func safeEnvironmentValue(value string) string {
	if strings.IndexByte(value, 0) >= 0 {
		return ""
	}
	return value
}

func pathError(cause error, message string) *pmuxerr.Error {
	return pmuxerr.Wrap(cause, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, message)
}

func helperError(cause error, message string) *pmuxerr.Error {
	return pmuxerr.Wrap(cause, pmuxerr.CodeDependencyMissing, pmuxerr.Environment, message)
}
