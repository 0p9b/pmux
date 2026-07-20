package pmuxerr

import "testing"

func TestExitCodeRegistry(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		CodeInternal: 1, CodeUsage: 2, CodeConfig: 3, CodeDependencyMissing: 4,
		CodeAuth: 5, CodeNetwork: 6, CodeUnhealthy: 7, CodeOwnershipConflict: 9,
		CodeCanceled: 10, CodeLaunchFailed: 125, CodeNotExecutable: 126,
		CodeExecutableMissing: 127, CodeInterrupted: 130,
	}
	for code, want := range cases {
		if got := ExitCode(New(code, User, code)); got != want {
			t.Errorf("ExitCode(%q) = %d, want %d", code, got, want)
		}
	}
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
	if got := ExitCode(New(InstallIntegrityFailed, Upstream, "bad checksum")); got != 7 {
		t.Fatalf("ExitCode(%q) = %d, want 7", InstallIntegrityFailed, got)
	}
}

func TestCapabilityMessage(t *testing.T) {
	t.Parallel()
	err := Capability("models", "7.2.91", "7.2.90")
	want := "This feature requires CLIProxyAPI ≥ 7.2.91 (detected: 7.2.90). Run `pmux update proxy` to upgrade."
	if err.Message != want {
		t.Fatalf("message = %q, want %q", err.Message, want)
	}
}
