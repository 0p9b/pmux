//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsRoots(t *testing.T) {
	values := map[string]string{
		"APPDATA":      `C:\Users\alice\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\alice\AppData\Local`,
	}
	p := newNative("")
	p.homeDir = func() (string, error) { return `C:\Users\alice`, nil }
	p.getenv = func(name string) string { return values[name] }

	assertWindowsRoot(t, p.ConfigDir, `C:\Users\alice\AppData\Roaming\PMux`)
	assertWindowsRoot(t, p.StateDir, `C:\Users\alice\AppData\Local\PMux\State`)
	assertWindowsRoot(t, p.CacheDir, `C:\Users\alice\AppData\Local\PMux\Cache`)
	assertWindowsRoot(t, p.DataDir, `C:\Users\alice\AppData\Local\PMux\Data`)
}

func TestWindowsRootFallbacks(t *testing.T) {
	p := newNative("")
	p.homeDir = func() (string, error) { return `C:\Users\alice`, nil }
	p.getenv = func(string) string { return "" }

	assertWindowsRoot(t, p.ConfigDir, `C:\Users\alice\AppData\Roaming\PMux`)
	assertWindowsRoot(t, p.StateDir, `C:\Users\alice\AppData\Local\PMux\State`)
}

func TestWindowsShell(t *testing.T) {
	p := newNative("")
	p.getenv = func(name string) string {
		if name == "COMSPEC" {
			return `C:\Windows\System32\cmd.exe`
		}
		return ""
	}
	if got := p.Shell(); got != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("Shell() = %q", got)
	}
	if p.IsWSL() {
		t.Fatal("Windows adapter reported WSL")
	}
}

func TestWindowsProtectedDACLCreateVerifyAndRepair(t *testing.T) {
	p := newNative("")
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(file, []byte("secret"), 0o666); err != nil {
		t.Fatal(err)
	}

	// Newly created paths inherit their parent's ACL and therefore should not
	// satisfy the protected current-user+SYSTEM-only contract.
	if err := p.VerifySecurePermissions(dir, true); err == nil {
		t.Fatal("VerifySecurePermissions accepted an inherited directory DACL")
	}
	if err := p.SecurePermissions(dir, true); err != nil {
		t.Fatalf("SecurePermissions(dir): %v", err)
	}
	if err := p.SecurePermissions(file, false); err != nil {
		t.Fatalf("SecurePermissions(file): %v", err)
	}
	if err := p.VerifySecurePermissions(dir, true); err != nil {
		t.Fatalf("VerifySecurePermissions(dir): %v", err)
	}
	if err := p.VerifySecurePermissions(file, false); err != nil {
		t.Fatalf("VerifySecurePermissions(file): %v", err)
	}

	// Applying again is the repair path and must be idempotent.
	if err := p.SecurePermissions(dir, true); err != nil {
		t.Fatalf("second SecurePermissions(dir): %v", err)
	}
}

func assertWindowsRoot(t *testing.T, root func() (string, error), want string) {
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
