package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrimPartialLinePreservesDurablePrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("first\nsecond\npartial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := TrimPartialLine(path); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "first\nsecond\n" {
		t.Fatalf("payload = %q", payload)
	}
}
