package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/app"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

func TestConfigEditorUsesPrivateSingleFileDirectArgvAndClassifiesRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only; production uses exec.CommandContext on every platform")
	}
	native, target := configEditFixture(t)
	observed := filepath.Join(t.TempDir(), "editor-argv")
	editor := writeEditor(t, "printf '%s\\n' \"$#\" \"$1\" > "+shellQuote(observed)+"\nsed 's/port: 8317/port: 9000/' \"$1\" > \"$1.next\" && mv \"$1.next\" \"$1\"")
	result, err := native.Edit(context.Background(), app.ConfigEditRequest{Scope: "proxy", Target: target, Editor: editor, Confirm: func(diff string) (bool, error) {
		if !strings.Contains(diff, "port") || strings.Contains(diff, "sk-1234567890abcdef") {
			t.Fatalf("diff was missing the change or leaked a secret: %s", diff)
		}
		return true, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.RestartRequired || result.BackupPath == "" {
		t.Fatalf("edit result=%#v", result)
	}
	argv, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 2 || lines[0] != "1" || filepath.Dir(lines[1]) != filepath.Dir(target) || lines[1] == target {
		t.Fatalf("editor argv=%q, want one same-directory temporary path", lines)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode=%v err=%v", info.Mode().Perm(), err)
	}
	body, _ := os.ReadFile(target)
	if !bytes.Contains(body, []byte("port: 9000")) {
		t.Fatalf("target was not committed: %s", body)
	}
}

func TestConfigEditorCancellationInvalidEditAndNoShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	t.Run("unchanged is cancellation", func(t *testing.T) {
		native, target := configEditFixture(t)
		before, _ := os.ReadFile(target)
		editor := writeEditor(t, ":")
		_, err := native.Edit(context.Background(), app.ConfigEditRequest{Scope: "proxy", Target: target, Editor: editor, Confirm: func(string) (bool, error) { return true, nil }})
		assertEditCanceledAndUnchanged(t, err, target, before)
	})
	t.Run("invalid yaml never commits", func(t *testing.T) {
		native, target := configEditFixture(t)
		before, _ := os.ReadFile(target)
		editor := writeEditor(t, "printf '%s\\n' 'host: [' > \"$1\"")
		_, err := native.Edit(context.Background(), app.ConfigEditRequest{Scope: "proxy", Target: target, Editor: editor, Confirm: func(string) (bool, error) { return true, nil }})
		if err == nil {
			t.Fatal("invalid edit unexpectedly succeeded")
		}
		after, _ := os.ReadFile(target)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid edit changed the target")
		}
	})
	t.Run("editor name is never shell evaluated", func(t *testing.T) {
		native, target := configEditFixture(t)
		marker := filepath.Join(t.TempDir(), "must-not-exist")
		_, err := native.Edit(context.Background(), app.ConfigEditRequest{Scope: "proxy", Target: target, Editor: "true;touch " + marker, Confirm: func(string) (bool, error) { return true, nil }})
		if err == nil {
			t.Fatal("shell-shaped editor unexpectedly succeeded")
		}
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("shell command was evaluated: %v", statErr)
		}
	})
}

func configEditFixture(t *testing.T) (*nativeRuntime, string) {
	t.Helper()
	root := t.TempDir()
	roots := domainplatform.Roots{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"), Cache: filepath.Join(root, "cache"), Data: filepath.Join(root, "data")}
	store, err := state.New(state.Paths{Config: filepath.Join(roots.Config, "config.json"), State: filepath.Join(roots.State, "state.json"), Secrets: filepath.Join(roots.State, "secrets.json")})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "instance", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "host: 127.0.0.1\nport: 8317\nauth-dir: " + filepath.Join(root, "auth") + "\napi-keys:\n  - sk-1234567890abcdef\nremote-management:\n  allow-remote: false\nws-auth: true\n"
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &nativeRuntime{roots: roots, store: store, stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, target
}

func writeEditor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertEditCanceledAndUnchanged(t *testing.T, err error, target string, before []byte) {
	t.Helper()
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.CodeCanceled {
		t.Fatalf("error=%#v, want cancellation", err)
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(before, after) {
		t.Fatal("canceled edit changed target")
	}
}

func TestPMuxConfigBackupRestoreIsPrivateChecksummedAndConflictSafe(t *testing.T) {
	native, _ := configEditFixture(t)
	original := state.Config{Version: state.SchemaVersion, Theme: "dark", LogLineLimit: 25}
	if err := native.store.SaveConfig(original); err != nil {
		t.Fatal(err)
	}
	id, err := native.BackupPMux(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(native.pmuxBackupDir(), id)
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%o", info.Mode().Perm())
	}
	changed := state.Config{Version: state.SchemaVersion, Theme: "light", LogLineLimit: 50}
	if err := native.store.SaveConfig(changed); err != nil {
		t.Fatal(err)
	}
	plan, err := native.PlanRestorePMux(context.Background(), id, changed)
	if err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, append(append([]byte(nil), backupBytes...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := native.RestorePMux(context.Background(), plan); err == nil {
		t.Fatal("changed backup unexpectedly restored")
	}
	current, err := native.store.LoadConfig()
	if err != nil || current.Theme != "light" {
		t.Fatalf("conflict changed current config: %#v err=%v", current, err)
	}
	if err := os.WriteFile(backupPath, backupBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err = native.PlanRestorePMux(context.Background(), id, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := native.RestorePMux(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	restored, err := native.store.LoadConfig()
	if err != nil || restored.Theme != "dark" || restored.LogLineLimit != 25 {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	backups, err := filepath.Glob(filepath.Join(native.pmuxBackupDir(), "config.json.*.bak"))
	if err != nil || len(backups) < 2 {
		t.Fatalf("restore did not privately back up current settings: %#v err=%v", backups, err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
