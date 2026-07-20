package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestConcurrentMutationRejectedAndReadOnlyUnaffected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "pmux.lock")
	manager, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.TryAcquire("first mutation")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	_, err = manager.TryAcquire("second mutation")
	if err == nil {
		t.Fatal("expected concurrent mutation rejection")
	}
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("error = %T %v, want BusyError", err, err)
	}
	if code := pmuxerr.ExitCode(err); code != 9 {
		t.Fatalf("contention exit code = %d, want 9", code)
	}
	if busy.Metadata.PID != os.Getpid() || busy.Metadata.Operation != "first mutation" {
		t.Fatalf("holder metadata = %#v", busy.Metadata)
	}

	// Read-only paths acquire no advisory lock and remain usable while a
	// mutation is active.
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("read-only access blocked by mutation lock: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := manager.TryAcquire("second mutation")
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHolderDeath(t *testing.T) {
	if os.Getenv("PMUX_LOCK_HELPER") == "1" {
		path := os.Getenv("PMUX_LOCK_PATH")
		ready := os.Getenv("PMUX_LOCK_READY")
		manager, err := New(path)
		if err != nil {
			os.Exit(2)
		}
		handle, err := manager.TryAcquire("holder-death-helper")
		if err != nil {
			os.Exit(3)
		}
		defer handle.Release()
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(time.Hour)
		return
	}

	root := t.TempDir()
	path := filepath.Join(root, "pmux.lock")
	ready := filepath.Join(root, "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestLockHolderDeath$")
	command.Env = append(os.Environ(), "PMUX_LOCK_HELPER=1", "PMUX_LOCK_PATH="+path, "PMUX_LOCK_READY="+ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	manager, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.TryAcquire("parent while helper alive"); err == nil {
		t.Fatal("lock was acquirable while helper lived")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = command.Process.Wait()

	var acquired *Handle
	deadline = time.Now().Add(5 * time.Second)
	for acquired == nil {
		acquired, err = manager.TryAcquire("parent after helper death")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("OS did not release lock after holder death: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := acquired.Release(); err != nil {
		t.Fatal(err)
	}
}
