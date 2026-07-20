//go:build !windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceHostUsesAbsoluteConfigRuntimeAndScrubbedEnv(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(root, "observed")
	binary := filepath.Join(root, "cli-proxy-api")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + observed + "\npwd >> " + observed + "\nprintf 'PGSTORE_URL=%s\\n' \"${PGSTORE_URL-}\" >> " + observed + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSTORE_URL", "postgres://must-not-reach-child")
	if err := RunServiceHost(context.Background(), []string{"--binary", binary, "--config", config, "--runtime-dir", runtimeDir}, ServiceHostStreams{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) >= 3 {
		lines[2] = normalizeTestPath(lines[2])
	}
	want := []string{"-config", config, normalizeTestPath(runtimeDir), "PGSTORE_URL="}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("service host observation = %q, want %q", lines, want)
	}
}

func TestServiceHostRejectsUnknownArguments(t *testing.T) {
	if err := RunServiceHost(context.Background(), []string{"--binary", "/tmp/core", "--config", "/tmp/config", "--unknown", "x"}, ServiceHostStreams{}); err == nil {
		t.Fatal("unknown private argument was accepted")
	}
}
