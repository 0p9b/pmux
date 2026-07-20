package runtime

import (
	"testing"

	"github.com/0p9b/pmux/internal/adapter/updater"
)

func TestSelfOwnershipRefusesPackageManagedLocations(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/usr/local/bin/pmux",
		"/opt/homebrew/bin/pmux",
		"/home/alice/go/bin/pmux",
		`C:\Users\Alice\scoop\apps\pmux\current\pmux.exe`,
	} {
		if got := selfOwnership(path); got != updater.OwnershipPackageManaged {
			t.Errorf("selfOwnership(%q) = %q", path, got)
		}
	}
	if got := selfOwnership("/home/alice/tools/pmux"); got != updater.OwnershipManaged {
		t.Fatalf("selfOwnership(manual release) = %q", got)
	}
}
