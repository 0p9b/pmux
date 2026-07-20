package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestAtomicWritePrivateDoesNotChangeExistingParentMode(t *testing.T) {
	root := t.TempDir()
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(foreign, "config.yaml")
	if err := AtomicWritePrivate(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWritePrivate(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "second" {
		t.Fatalf("payload = %q", payload)
	}
	parentInfo, err := os.Stat(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && parentInfo.Mode().Perm() != 0o755 {
		t.Fatalf("foreign parent mode changed to %o", parentInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private file mode = %o", fileInfo.Mode().Perm())
	}
}

func TestBackupRetentionIsDeterministicAndExplicit(t *testing.T) {
	manager, err := NewBackups(filepath.Join(t.TempDir(), "backups"), 3)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	var created []string
	for i := range 5 {
		manager.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		path, err := manager.Create("default", "config.yaml", []byte(fmt.Sprintf("payload-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, path)
	}
	before, err := manager.List("default", "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 5 {
		t.Fatalf("Create pruned before verification: got %d backups", len(before))
	}
	removed, err := manager.Prune("default", "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{created[1], created[0]}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("removed = %#v, want %#v", removed, wantRemoved)
	}
	after, err := manager.List("default", "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := []string{created[4], created[3], created[2]}
	if !reflect.DeepEqual(after, wantAfter) {
		t.Fatalf("retained = %#v, want %#v", after, wantAfter)
	}
	for _, path := range after {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("backup %s mode = %o", path, info.Mode().Perm())
		}
	}
}

func TestWritePrivateExclusiveRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "value")
	if err := WritePrivateExclusive(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateExclusive(path, []byte("new")); err == nil {
		t.Fatal("expected overwrite conflict")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "old" {
		t.Fatalf("existing file changed to %q", payload)
	}
}
