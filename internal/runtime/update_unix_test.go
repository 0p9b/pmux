//go:build !windows

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedProxyBinaryUsesCurrentPointer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	versionDir := filepath.Join(root, "versions", "7.2.92")
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(versionDir, "cli-proxy-api")
	if err := os.WriteFile(binary, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "current")
	if err := os.Symlink(filepath.Join("versions", "7.2.92"), current); err != nil {
		t.Fatal(err)
	}
	gotBinary, gotVersion, err := selectedProxyBinary(current)
	if err != nil {
		t.Fatal(err)
	}
	if gotBinary != binary || gotVersion != "7.2.92" {
		t.Fatalf("selectedProxyBinary() = (%q, %q), want (%q, 7.2.92)", gotBinary, gotVersion, binary)
	}
}
