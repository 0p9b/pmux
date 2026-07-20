package journal

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	domain "github.com/0p9b/pmux/internal/domain/journal"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestPendingTransactionSurvivesReopenAndClosesAfterCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.jsonl")
	first, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.Begin("pmux setup --mode managed", map[string]string{"instance": "default"})
	if err != nil {
		t.Fatal(err)
	}
	stepTime := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if err := first.Record(id, domain.Step{Name: "write config", Action: "write_file", Target: "/private/config.yaml", At: stepTime}); err != nil {
		t.Fatal(err)
	}
	if err := first.Interrupt(id, "signal received before next commit"); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count = %d", len(pending))
	}
	if pending[0].ID != id || pending[0].State != StateInterrupted || len(pending[0].Steps) != 1 {
		t.Fatalf("recovered transaction = %#v", pending[0])
	}
	if !pending[0].Steps[0].At.Equal(stepTime) {
		t.Fatalf("step time changed: %v", pending[0].Steps[0].At)
	}
	if err := reopened.Commit(id); err != nil {
		t.Fatal(err)
	}
	pending, err = reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("committed transaction remained pending: %#v", pending)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o", info.Mode().Perm())
	}
}

func TestJournalDropsTornTrailingRecordDuringRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	journal, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := journal.Begin("operation", nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":1,"sequence":2`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("valid durable prefix not recovered: %#v", pending)
	}
	if err := reopened.Record(id, domain.Step{Name: "resume", Action: "verify"}); err != nil {
		t.Fatalf("append after torn-tail recovery: %v", err)
	}
	pending, err = reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || len(pending[0].Steps) != 1 {
		t.Fatalf("journal was not writable after recovery: %#v", pending)
	}
}

func TestJournalRejectsCorruptCompleteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.Pending()
	if err == nil {
		t.Fatal("expected corruption error")
	}
	var pmuxError *pmuxerr.Error
	if !errors.As(err, &pmuxError) || pmuxError.Code != pmuxerr.JournalCorrupt {
		t.Fatalf("error = %#v", err)
	}
}

func TestJournalNeverPersistsKnownSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	secret := "sk-CANARY-VERY-SECRET-VALUE"
	journal, err := New(path, WithKnownSecrets(secret))
	if err != nil {
		t.Fatal(err)
	}
	id, err := journal.Begin("rotate "+secret, map[string]string{"api_key": secret, "note": "replace " + secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(id, domain.Step{Name: "replace " + secret, Action: "write", Target: "/safe/path", Undo: map[string]string{"token": secret}}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret) {
		t.Fatalf("complete secret persisted: %s", payload)
	}
	if !strings.Contains(string(payload), "<redacted>") && !strings.Contains(string(payload), "…") {
		t.Fatalf("expected redaction marker: %s", payload)
	}
}
