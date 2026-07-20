package bundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBundleIsPrivateRedactedAndExcludesAuthContents(t *testing.T) {
	root := t.TempDir()
	authRoot := filepath.Join(root, "auth")
	if err := os.Mkdir(authRoot, 0o700); err != nil { t.Fatal(err) }
	canary := "CANARY-secret-value-123456"
	bareSecret := "unseeded-yaml-secret-654321"
	pemCanary := "-----BEGIN PRIVATE KEY-----\ncanary-private-material\n-----END PRIVATE KEY-----"
	destination := filepath.Join(root, "doctor.zip")
	builder := Builder{
		AuthRoots: []string{authRoot},
		KnownSecrets: []string{canary},
		Now: func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	}
	result, err := builder.Build(t.Context(), destination, []Entry{
		{ArchivePath: "doctor.json", SourcePath: filepath.Join(root, "doctor.json"), Kind: KindJSON, Data: []byte(`{"message":"` + canary + `","authorization":"Bearer abcdefghijklmnop"}`)},
		{ArchivePath: "logs/pmux.log", SourcePath: filepath.Join(root, "pmux.log"), Kind: KindText, Data: []byte("ANTHROPIC_AUTH_TOKEN=" + canary + "\nkey sk-abcdefghijklmnop\nsecret-key: " + bareSecret + "\n" + pemCanary + "\n")},
		{ArchivePath: "auth/account.json", SourcePath: filepath.Join(authRoot, "codex-user.json"), Kind: KindJSON, Data: []byte("AUTH-CONTENT-DO-NOT-INCLUDE " + canary)},
		{ArchivePath: "renamed-auth.txt", SourcePath: filepath.Join(root, "elsewhere.json"), Kind: KindAuthFile, Data: []byte("AUTH-CONTENT-DO-NOT-INCLUDE")},
	})
	if err != nil { t.Fatal(err) }
	if result.Path != destination { t.Fatalf("path = %q", result.Path) }
	info, err := os.Stat(destination)
	if err != nil { t.Fatal(err) }
	if info.Mode().Perm() != 0o600 { t.Fatalf("mode = %o", info.Mode().Perm()) }
	if result.Manifest.Excluded["auth-file-content"] != 2 { t.Fatalf("excluded = %#v", result.Manifest.Excluded) }

	files := readZip(t, destination)
	if _, ok := files["auth/account.json"]; ok { t.Fatal("auth file was included") }
	if _, ok := files["renamed-auth.txt"]; ok { t.Fatal("kind-marked auth file was included") }
	for name, data := range files {
		if bytes.Contains(data, []byte(canary)) { t.Fatalf("canary in %s", name) }
		if bytes.Contains(data, []byte("AUTH-CONTENT-DO-NOT-INCLUDE")) { t.Fatalf("auth content in %s", name) }
		if bytes.Contains(data, []byte("sk-abcdefghijklmnop")) { t.Fatalf("proxy key in %s", name) }
		if bytes.Contains(data, []byte(bareSecret)) { t.Fatalf("unseeded structured secret in %s", name) }
		if bytes.Contains(data, []byte("canary-private-material")) { t.Fatalf("PEM private key in %s", name) }
	}
	if !bytes.Contains(files["doctor.json"], []byte("<redacted>")) { t.Fatalf("doctor was not redacted: %s", files["doctor.json"]) }
	var manifest Manifest
	if err := json.Unmarshal(files["MANIFEST.json"], &manifest); err != nil { t.Fatal(err) }
	if len(manifest.Entries) != 2 { t.Fatalf("manifest entries = %d", len(manifest.Entries)) }
}

func TestBundleUsesPermissionAndVerificationHooks(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "doctor.zip")
	secureCalled, verifyCalled := false, false
	builder := Builder{
		SecurePermissions: func(path string, isDir bool) error {
			secureCalled = path == destination && !isDir
			return os.Chmod(path, 0o600)
		},
		VerifySecurePermissions: func(path string, isDir bool) error {
			verifyCalled = path == destination && !isDir
			info, err := os.Stat(path)
			if err != nil { return err }
			if info.Mode().Perm() != 0o600 { return os.ErrPermission }
			return nil
		},
	}
	if _, err := builder.Build(t.Context(), destination, []Entry{{ArchivePath: "doctor.json", Kind: KindJSON, Data: []byte(`{"ok":true}`)}}); err != nil { t.Fatal(err) }
	if !secureCalled || !verifyCalled { t.Fatalf("hooks not called: secure=%t verify=%t", secureCalled, verifyCalled) }
}

func TestBundleHasNoAuthOverride(t *testing.T) {
	typeOf := reflect.TypeOf(Builder{})
	for i := range typeOf.NumField() {
		name := strings.ToLower(typeOf.Field(i).Name)
		if strings.Contains(name, "includeauth") || strings.Contains(name, "allowauth") || strings.Contains(name, "override") {
			t.Fatalf("auth exclusion override exists: %s", typeOf.Field(i).Name)
		}
	}
}

func TestBundleRefusesExistingDestinationAndUnsafePaths(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "doctor.zip")
	if err := os.WriteFile(destination, []byte("keep"), 0o600); err != nil { t.Fatal(err) }
	_, err := (Builder{}).Build(t.Context(), destination, nil)
	if err == nil { t.Fatal("expected destination conflict") }
	data, _ := os.ReadFile(destination)
	if string(data) != "keep" { t.Fatalf("existing destination changed: %q", data) }

	unsafe := filepath.Join(root, "unsafe.zip")
	_, err = (Builder{}).Build(t.Context(), unsafe, []Entry{{ArchivePath: "../escape", Data: []byte("x")}})
	if err == nil { t.Fatal("expected unsafe path error") }
	if _, statErr := os.Stat(unsafe); !os.IsNotExist(statErr) { t.Fatalf("unsafe archive was created: %v", statErr) }
}

func TestBundleRejectsCanaryInArchiveFilename(t *testing.T) {
	root := t.TempDir()
	canary := "CANARY-FILENAME-SECRET"
	destination := filepath.Join(root, "doctor.zip")
	_, err := (Builder{KnownSecrets: []string{canary}}).Build(t.Context(), destination, []Entry{{ArchivePath: canary + ".log", Data: []byte("safe")}})
	if err == nil { t.Fatal("expected filename canary rejection") }
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) { t.Fatalf("archive was created: %v", statErr) }
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil { t.Fatal(err) }
	defer reader.Close()
	out := make(map[string][]byte)
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil { t.Fatal(err) }
		data, err := io.ReadAll(rc)
		if err != nil { t.Fatal(err) }
		if err := rc.Close(); err != nil { t.Fatal(err) }
		out[file.Name] = data
	}
	return out
}
