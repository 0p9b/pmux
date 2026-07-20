//go:build linux

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxDefaultRoots(t *testing.T) {
	p := newNative("")
	p.homeDir = func() (string, error) { return "/home/alice", nil }
	p.getenv = func(string) string { return "" }

	assertRoot(t, p.ConfigDir, "/home/alice/.config/pmux")
	assertRoot(t, p.StateDir, "/home/alice/.local/state/pmux")
	assertRoot(t, p.CacheDir, "/home/alice/.cache/pmux")
	assertRoot(t, p.DataDir, "/home/alice/.local/share/pmux")
}

func TestLinuxXDGRootsAndConfigOverride(t *testing.T) {
	values := map[string]string{
		"XDG_CONFIG_HOME": "/xdg/config",
		"XDG_STATE_HOME":  "/xdg/state",
		"XDG_CACHE_HOME":  "/xdg/cache",
		"XDG_DATA_HOME":   "/xdg/data",
	}
	p := newNative("/override/config")
	p.homeDir = func() (string, error) { return "/home/alice", nil }
	p.getenv = func(name string) string { return values[name] }

	assertRoot(t, p.ConfigDir, "/override/config")
	assertRoot(t, p.StateDir, "/xdg/state/pmux")
	assertRoot(t, p.CacheDir, "/xdg/cache/pmux")
	assertRoot(t, p.DataDir, "/xdg/data/pmux")
}

func TestLinuxIgnoresRelativeXDGRoots(t *testing.T) {
	p := newNative("")
	p.homeDir = func() (string, error) { return "/home/alice", nil }
	p.getenv = func(string) string { return "relative" }
	assertRoot(t, p.ConfigDir, "/home/alice/.config/pmux")
}

func TestLinuxHomeFailureIsWrapped(t *testing.T) {
	p := newNative("")
	p.homeDir = func() (string, error) { return "", errors.New("lookup failed") }
	if _, err := p.ConfigDir(); err == nil {
		t.Fatal("ConfigDir succeeded with an unavailable home")
	}
}

func TestLinuxWSLDetection(t *testing.T) {
	tests := []struct {
		name     string
		distro   string
		contents map[string]string
		want     bool
	}{
		{name: "environment", distro: "Ubuntu", want: true},
		{name: "osrelease", contents: map[string]string{"/proc/sys/kernel/osrelease": "6.1.0-microsoft-standard-WSL2"}, want: true},
		{name: "proc version", contents: map[string]string{"/proc/version": "Linux version 5.15.0 Microsoft"}, want: true},
		{name: "native Linux", contents: map[string]string{"/proc/sys/kernel/osrelease": "6.12.0-amd64", "/proc/version": "Linux version 6.12.0"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newNative("")
			p.getenv = func(name string) string {
				if name == "WSL_DISTRO_NAME" {
					return test.distro
				}
				return ""
			}
			p.readFile = func(path string) ([]byte, error) {
				if contents, ok := test.contents[path]; ok {
					return []byte(contents), nil
				}
				return nil, os.ErrNotExist
			}
			if got := p.IsWSL(); got != test.want {
				t.Fatalf("IsWSL() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLinuxShell(t *testing.T) {
	p := newNative("")
	p.getenv = func(name string) string {
		if name == "SHELL" {
			return "/bin/zsh"
		}
		return ""
	}
	if got := p.Shell(); got != "/bin/zsh" {
		t.Fatalf("Shell() = %q, want /bin/zsh", got)
	}
	p.getenv = func(string) string { return "" }
	if got := p.Shell(); got != "/bin/sh" {
		t.Fatalf("fallback Shell() = %q, want /bin/sh", got)
	}
}

func TestUnixSecurePermissions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(file, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newNative("")
	if err := p.SecurePermissions(dir, true); err != nil {
		t.Fatalf("SecurePermissions(dir): %v", err)
	}
	if err := p.SecurePermissions(file, false); err != nil {
		t.Fatalf("SecurePermissions(file): %v", err)
	}
	assertMode(t, dir, 0o700)
	assertMode(t, file, 0o600)
	if err := p.VerifySecurePermissions(dir, true); err != nil {
		t.Fatalf("VerifySecurePermissions(dir): %v", err)
	}
	if err := p.VerifySecurePermissions(file, false); err != nil {
		t.Fatalf("VerifySecurePermissions(file): %v", err)
	}
}

func TestUnixPermissionVerificationRejectsDriftAndSymlinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "secret")
	if err := os.WriteFile(file, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newNative("")
	if err := p.VerifySecurePermissions(file, false); err == nil {
		t.Fatal("VerifySecurePermissions accepted mode 0644")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := p.SecurePermissions(link, false); err == nil {
		t.Fatal("SecurePermissions followed a symbolic link")
	}
}

func TestOpenBrowserUsesResolvedHelperWithoutShell(t *testing.T) {
	p := newNative("")
	p.getenv = func(string) string { return "" }
	p.readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	p.lookPath = func(string) (string, error) { return "/bin/true", nil }
	if err := p.OpenBrowser(t.Context(), "https://example.invalid/a?b=c"); err != nil {
		t.Fatalf("OpenBrowser: %v", err)
	}
}

func assertRoot(t *testing.T, root func() (string, error), want string) {
	t.Helper()
	got, err := root()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("root %q is not absolute", got)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}
