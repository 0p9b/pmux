//go:build windows

package updater

import (
	"os"
	"testing"
)

// TestMain prevents the detached self-update helper from launching the test
// binary itself during engine tests.
func TestMain(m *testing.M) {
	selfHelperLauncher = func(string, string) error { return nil }
	os.Exit(m.Run())
}
