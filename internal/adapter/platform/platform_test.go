package platform

import (
	"path/filepath"
	"testing"

	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
)

func TestFactoryImplementsDomainPlatform(t *testing.T) {
	adapter, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var _ domainplatform.Platform = adapter //nolint:staticcheck
	if adapter.GOOS() == "" {
		t.Fatal("factory returned an adapter without an operating-system identifier")
	}
}

func TestResolveConfigOverride(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	got, err := ResolveConfigOverride(filepath.Join("relative", "config"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "relative", "config")
	if got != want {
		t.Fatalf("ResolveConfigOverride() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("resolved config override %q is not absolute", got)
	}

	empty, err := ResolveConfigOverride("")
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Fatalf("empty override resolved to %q", empty)
	}
}

func TestFactoryRejectsMultipleOverrides(t *testing.T) {
	if _, err := New("first", "second"); err == nil {
		t.Fatal("New accepted multiple config overrides")
	}
}
