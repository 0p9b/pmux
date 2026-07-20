//go:build darwin

package platform

import "testing"

func TestDarwinRoots(t *testing.T) {
	p := newNative("")
	p.homeDir = func() (string, error) { return "/Users/alice", nil }

	assertDarwinRoot(t, p.ConfigDir, "/Users/alice/Library/Application Support/PMux")
	assertDarwinRoot(t, p.StateDir, "/Users/alice/Library/Application Support/PMux/State")
	assertDarwinRoot(t, p.CacheDir, "/Users/alice/Library/Caches/PMux")
	assertDarwinRoot(t, p.DataDir, "/Users/alice/Library/Application Support/PMux")
}

func TestDarwinShell(t *testing.T) {
	p := newNative("")
	p.getenv = func(name string) string {
		if name == "SHELL" {
			return "/bin/zsh"
		}
		return ""
	}
	if got := p.Shell(); got != "/bin/zsh" {
		t.Fatalf("Shell() = %q", got)
	}
	if p.IsWSL() {
		t.Fatal("macOS adapter reported WSL")
	}
}

func assertDarwinRoot(t *testing.T, root func() (string, error), want string) {
	t.Helper()
	got, err := root()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}
