//go:build darwin

package discovery

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinProcessName(t *testing.T) {
	value := [17]byte{'c', 'l', 'i', '-', 'p', 'r', 'o', 'x', 'y', 0, 'x'}
	if got := darwinProcessName(value[:]); got != "cli-proxy" {
		t.Fatalf("darwinProcessName = %q", got)
	}
}

func TestDarwinProcArgsCurrentProcess(t *testing.T) {
	raw, err := unix.SysctlRaw("kern.procargs2", unix.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	executable, argv, err := parseDarwinProcArgs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if executable == "" || len(argv) == 0 {
		t.Fatalf("executable = %q, argv = %#v", executable, argv)
	}
}
