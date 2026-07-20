package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAuditAppendPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "audit.jsonl")
	log, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{Operation: "op_1", Actor: "local", Command: "pmux service restart", Target: "default", Result: ResultOK}
	if err := log.Append(entry); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Operation != entry.Operation || entries[0].Result != ResultOK || entries[0].Timestamp.IsZero() {
		t.Fatalf("entries = %#v", entries)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode = %o", info.Mode().Perm())
	}
}

func TestAuditNeverPersistsCompleteSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	secret := "sk-CANARY-VERY-SECRET-VALUE"
	log, err := New(path, WithKnownSecrets(secret))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{
		Operation: "op_2", Actor: "local", Command: "pmux config set note " + secret, Target: "config.yaml",
		Params: map[string]string{"api_key": secret, "note": "replaced " + secret}, Result: ResultFailed, ErrorCode: "PMUX-2002",
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret) {
		t.Fatalf("complete secret persisted: %s", payload)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Params["api_key"] == secret {
		t.Fatalf("audit params not sanitized: %#v", entries)
	}
}

func TestAuditIgnoresTornTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Operation: "op_3", Actor: "local", Command: "pmux setup", Target: "default", Result: ResultOK}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":1`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("durable prefix not recovered: %#v", entries)
	}
	if err := log.Append(Entry{Operation: "op_4", Actor: "local", Command: "pmux doctor --fix", Target: "default", Result: ResultOK}); err != nil {
		t.Fatalf("append after torn-tail recovery: %v", err)
	}
	entries, err = log.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit log was not writable after recovery: %#v", entries)
	}
}
