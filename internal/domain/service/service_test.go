package service

import "testing"

func TestCanonicalIdentities(t *testing.T) {
	t.Parallel()
	cases := map[ServiceBackend]string{
		BackendSystemdUser: "pmux-cliproxyapi@default.service",
		BackendLaunchd:     "dev.pmux.cliproxyapi.default",
		BackendWindowsTask: "PMux CLIProxyAPI (default)",
	}
	for backend, want := range cases {
		if got := Identity(backend, "default"); got != want {
			t.Errorf("Identity(%q) = %q, want %q", backend, got, want)
		}
	}
}
