package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapterlock "github.com/0p9b/pmux/internal/adapter/lock"
	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/audit"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestMutationClassifierCoversEveryPublicMutationClass(t *testing.T) {
	t.Parallel()
	mutations := []app.Invocation{
		{Operation: app.OpSetup},
		{Operation: app.OpProvidersLogin},
		{Operation: app.OpProvidersEnable},
		{Operation: app.OpProvidersDisable},
		{Operation: app.OpProvidersRemove},
		{Operation: app.OpModelsFavorite},
		{Operation: app.OpModelsUnfavorite},
		{Operation: app.OpDoctor, Options: map[string]any{"fix": true}},
		{Operation: app.OpDoctor, Options: map[string]any{"bundle": "private-canary-path"}},
		{Operation: app.OpServiceStart},
		{Operation: app.OpServiceStop},
		{Operation: app.OpServiceRestart},
		{Operation: app.OpServiceInstall},
		{Operation: app.OpServiceUninstall},
		{Operation: app.OpServiceLogs, Options: map[string]any{"clear": "proxy"}},
		{Operation: app.OpServiceLogs, Options: map[string]any{"output": "private-canary-path"}},
		{Operation: app.OpConfigSet},
		{Operation: app.OpConfigEdit},
		{Operation: app.OpConfigBackup},
		{Operation: app.OpConfigRestore},
		{Operation: app.OpUpdateSelf},
		{Operation: app.OpUpdateProxy},
	}
	for _, invocation := range mutations {
		if !IsMutation(invocation) {
			t.Errorf("%s was classified read-only", invocation.Operation)
		}
	}

	readOnly := []app.Invocation{
		{Operation: app.OpDashboardStatus},
		{Operation: app.OpTUIDashboard},
		{Operation: app.OpProvidersList},
		{Operation: app.OpProvidersVerify},
		{Operation: app.OpTUIProviders},
		{Operation: app.OpModelsList},
		{Operation: app.OpModelsTest},
		{Operation: app.OpTUIModels},
		{Operation: app.OpLaunch},
		{Operation: app.OpLaunchPreflight},
		{Operation: app.OpDoctor},
		{Operation: app.OpServiceStatus},
		{Operation: app.OpTUIService},
		{Operation: app.OpServiceLogs},
		{Operation: app.OpServiceLogs, Options: map[string]any{"clear": "  "}},
		{Operation: app.OpConfigShow},
		{Operation: app.OpConfigGet},
		{Operation: app.OpTUIConfig},
		{Operation: app.OpUpdateCheck},
		{Operation: app.Operation("unknown.operation")},
	}
	for _, invocation := range readOnly {
		if IsMutation(invocation) {
			t.Errorf("%s was classified as a mutation", invocation.Operation)
		}
	}
}

func TestGovernanceReadOnlyCommandsNeverAcquireOrWrite(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	inner := app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		calls.Add(1)
		return app.Result{Data: "read"}, nil
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := []app.Invocation{
		{Operation: app.OpDashboardStatus},
		{Operation: app.OpProvidersList},
		{Operation: app.OpProvidersVerify},
		{Operation: app.OpModelsList, Options: map[string]any{"refresh": true}},
		{Operation: app.OpModelsTest},
		{Operation: app.OpLaunch},
		{Operation: app.OpDoctor},
		{Operation: app.OpServiceStatus},
		{Operation: app.OpServiceLogs, Options: map[string]any{"follow": true}},
		{Operation: app.OpConfigShow},
		{Operation: app.OpConfigGet},
		{Operation: app.OpUpdateCheck},
	}
	for _, invocation := range readOnly {
		if _, err := governed.Execute(context.Background(), invocation, nil); err != nil {
			t.Fatalf("%s: %v", invocation.Operation, err)
		}
	}
	if got := calls.Load(); got != int32(len(readOnly)) {
		t.Fatalf("inner calls = %d, want %d", got, len(readOnly))
	}
	for _, name := range []string{"pmux.lock", "journal.jsonl", "audit.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read-only commands created %s: %v", name, err)
		}
	}
}

func TestExportMutationsUseSafeTargetsAndRespectContention(t *testing.T) {
	for _, test := range []struct {
		name       string
		invocation app.Invocation
		target     string
	}{
		{name: "doctor bundle", invocation: app.Invocation{Operation: app.OpDoctor, Options: map[string]any{"bundle": "secret-bundle-path"}}, target: "diagnostics"},
		{name: "service log output", invocation: app.Invocation{Operation: app.OpServiceLogs, Options: map[string]any{"output": "secret-log-path"}}, target: "service"},
	} {
		t.Run(test.name+" success", func(t *testing.T) {
			root := t.TempDir()
			var calls atomic.Int32
			governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
				calls.Add(1)
				return app.Result{}, nil
			}), root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := governed.Execute(context.Background(), test.invocation, nil); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("inner calls = %d", calls.Load())
			}
			events := readJSONLines(t, filepath.Join(root, "journal.jsonl"))
			step, ok := events[1]["step"].(map[string]any)
			if !ok || step["target"] != test.target {
				t.Fatalf("journal target = %#v", events[1]["step"])
			}
			entries := readAudit(t, root)
			if len(entries) != 1 || entries[0].Target != test.target || entries[0].Command != string(test.invocation.Operation) {
				t.Fatalf("audit entries = %#v", entries)
			}
			assertGovernanceExcludes(t, root, "secret-bundle-path", "secret-log-path")
		})

		t.Run(test.name+" contention", func(t *testing.T) {
			root := t.TempDir()
			manager, err := adapterlock.New(filepath.Join(root, "pmux.lock"))
			if err != nil {
				t.Fatal(err)
			}
			holder, err := manager.TryAcquire("existing-export")
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Release()
			var calls atomic.Int32
			governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
				calls.Add(1)
				return app.Result{}, nil
			}), root)
			if err != nil {
				t.Fatal(err)
			}
			_, got := governed.Execute(context.Background(), test.invocation, nil)
			if got == nil || pmuxerr.ExitCode(got) != 9 || calls.Load() != 0 {
				t.Fatalf("contended export calls=%d err=%#v", calls.Load(), got)
			}
			for _, name := range []string{"journal.jsonl", "audit.jsonl"} {
				if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("contention created %s: %v", name, err)
				}
			}
		})
	}
}

func TestGovernedMutationSuccessWritesCompletedJournalAndOneAudit(t *testing.T) {
	root := t.TempDir()
	const canary = "secret-canary-success"
	var received app.Invocation
	inner := app.UseCaseFunc(func(_ context.Context, invocation app.Invocation, _ app.EventSink) (app.Result, error) {
		received = invocation
		return app.Result{Data: "done"}, nil
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	invocation := app.Invocation{Operation: app.OpConfigSet, Arguments: []string{canary}, Options: map[string]any{"path": canary}}
	result, err := governed.Execute(context.Background(), invocation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Data != "done" || received.Arguments[0] != canary {
		t.Fatalf("inner invocation/result changed: %#v %#v", received, result)
	}

	events := readJSONLines(t, filepath.Join(root, "journal.jsonl"))
	if len(events) != 3 || events[0]["type"] != "begin" || events[1]["type"] != "step" || events[2]["state"] != "completed" {
		t.Fatalf("journal events = %#v", events)
	}
	if events[0]["operation"] != string(app.OpConfigSet) || events[0]["metadata"] != nil {
		t.Fatalf("unsafe begin event = %#v", events[0])
	}
	step, ok := events[1]["step"].(map[string]any)
	if !ok || step["name"] != governanceDispatch || step["action"] != governanceExecute || step["target"] != "configuration" {
		t.Fatalf("dispatch step = %#v", events[1]["step"])
	}
	auditEntries := readAudit(t, root)
	if len(auditEntries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(auditEntries))
	}
	entry := auditEntries[0]
	if entry.Operation == "" || entry.Operation != events[0]["tx_id"] || entry.Actor != governanceActor || entry.Command != string(app.OpConfigSet) || entry.Target != "configuration" || entry.Result != audit.ResultOK || entry.ErrorCode != "" || entry.Params != nil {
		t.Fatalf("audit entry = %#v", entry)
	}
	assertGovernanceExcludes(t, root, canary)
}

func TestGovernedMutationFailurePreservesBusinessErrorAndRedactsRecords(t *testing.T) {
	root := t.TempDir()
	const canary = "secret-canary-business-error"
	want := pmuxerr.New(pmuxerr.ManagementAuthRejected, pmuxerr.Upstream, "provider rejected "+canary)
	inner := app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		return app.Result{Data: "partial"}, want
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	result, got := governed.Execute(context.Background(), app.Invocation{Operation: app.OpProvidersLogin, Arguments: []string{canary}, Options: map[string]any{"token": canary}}, nil)
	if got != want {
		t.Fatalf("business error identity was not preserved: got %#v want %#v", got, want)
	}
	if result.Data != "partial" {
		t.Fatalf("partial result = %#v", result)
	}
	events := readJSONLines(t, filepath.Join(root, "journal.jsonl"))
	if len(events) != 3 || events[2]["state"] != "failed" || events[2]["reason"] != governanceFailed {
		t.Fatalf("journal events = %#v", events)
	}
	entries := readAudit(t, root)
	if len(entries) != 1 || entries[0].Result != audit.ResultFailed || entries[0].ErrorCode != pmuxerr.ManagementAuthRejected {
		t.Fatalf("audit entries = %#v", entries)
	}
	assertGovernanceExcludes(t, root, canary)
}

func TestGovernedMutationNonzeroResultIsFailedWithoutChangingResultContract(t *testing.T) {
	root := t.TempDir()
	inner := app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		return app.Result{Data: "unresolved", ExitCode: 7}, nil
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	result, got := governed.Execute(context.Background(), app.Invocation{Operation: app.OpDoctor, Options: map[string]any{"fix": true}}, nil)
	if got != nil || result.Data != "unresolved" || result.ExitCode != 7 {
		t.Fatalf("result contract changed: result=%#v err=%#v", result, got)
	}
	events := readJSONLines(t, filepath.Join(root, "journal.jsonl"))
	if len(events) != 3 || events[2]["state"] != "failed" {
		t.Fatalf("journal events = %#v", events)
	}
	entries := readAudit(t, root)
	if len(entries) != 1 || entries[0].Result != audit.ResultFailed || entries[0].ErrorCode != pmuxerr.CodeUnhealthy {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestGovernedMutationLockContentionRejectsBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	manager, err := adapterlock.New(filepath.Join(root, "pmux.lock"))
	if err != nil {
		t.Fatal(err)
	}
	holder, err := manager.TryAcquire("existing-operation")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()

	var calls atomic.Int32
	governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		calls.Add(1)
		return app.Result{}, nil
	}), root)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, got := governed.Execute(context.Background(), app.Invocation{Operation: app.OpServiceRestart}, nil)
	if got == nil || pmuxerr.ExitCode(got) != 9 {
		t.Fatalf("contention error = %#v, exit=%d", got, pmuxerr.ExitCode(got))
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("TryAcquire waited for %s", elapsed)
	}
	var busy *adapterlock.BusyError
	if !errors.As(got, &busy) || busy.Metadata.Operation != "existing-operation" {
		t.Fatalf("trusted holder metadata missing: %#v", got)
	}
	if calls.Load() != 0 {
		t.Fatal("inner mutation dispatched during lock contention")
	}
	for _, name := range []string{"journal.jsonl", "audit.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("contention created %s: %v", name, err)
		}
	}
}

func TestConcurrentGovernedMutationsRejectSecondAndCompleteFirst(t *testing.T) {
	root := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	inner := app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return app.Result{}, nil
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := governed.Execute(context.Background(), app.Invocation{Operation: app.OpModelsFavorite}, nil)
		firstDone <- err
	}()
	<-entered
	_, secondErr := governed.Execute(context.Background(), app.Invocation{Operation: app.OpModelsUnfavorite}, nil)
	if secondErr == nil || pmuxerr.ExitCode(secondErr) != 9 {
		t.Fatalf("second mutation error = %#v", secondErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent dispatch count = %d", calls.Load())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if entries := readAudit(t, root); len(entries) != 1 || entries[0].Result != audit.ResultOK {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestForegroundAttachmentRunsAfterStartupGovernanceReleasesLock(t *testing.T) {
	root := t.TempDir()
	attachmentStarted := make(chan struct{})
	releaseAttachment := make(chan struct{})
	var startCalls atomic.Int32
	var stopCalls atomic.Int32
	inner := app.UseCaseFunc(func(_ context.Context, invocation app.Invocation, _ app.EventSink) (app.Result, error) {
		switch invocation.Operation {
		case app.OpServiceStart:
			startCalls.Add(1)
			return app.Result{Attachment: func(context.Context) error {
				close(attachmentStarted)
				<-releaseAttachment
				return nil
			}}, nil
		case app.OpServiceStop:
			stopCalls.Add(1)
			return app.Result{}, nil
		default:
			t.Fatalf("unexpected operation %s", invocation.Operation)
			return app.Result{}, nil
		}
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	startResult, err := governed.Execute(context.Background(), app.Invocation{Operation: app.OpServiceStart, Options: map[string]any{"foreground": true}}, nil)
	if err != nil || startResult.Attachment == nil || startCalls.Load() != 1 {
		t.Fatalf("foreground startup result=%#v calls=%d err=%v", startResult, startCalls.Load(), err)
	}
	if entries := readAudit(t, root); len(entries) != 1 || entries[0].Command != string(app.OpServiceStart) || entries[0].Result != audit.ResultOK {
		t.Fatalf("startup audit entries = %#v", entries)
	}
	attachmentDone := make(chan error, 1)
	go func() { attachmentDone <- startResult.Attachment(context.Background()) }()
	<-attachmentStarted
	if _, err := governed.Execute(context.Background(), app.Invocation{Operation: app.OpServiceStop, Yes: true}, nil); err != nil {
		t.Fatalf("stop was blocked by attached foreground wait: %v", err)
	}
	if stopCalls.Load() != 1 {
		t.Fatalf("stop calls = %d", stopCalls.Load())
	}
	close(releaseAttachment)
	if err := <-attachmentDone; err != nil {
		t.Fatal(err)
	}
	if entries := readAudit(t, root); len(entries) != 2 || entries[1].Command != string(app.OpServiceStop) || entries[1].Result != audit.ResultOK {
		t.Fatalf("lifecycle audit entries = %#v", entries)
	}
}

func TestGovernedMutationHonorsCancellationBeforeAcquire(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		calls.Add(1)
		return app.Result{}, nil
	}), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, got := governed.Execute(ctx, app.Invocation{Operation: app.OpUpdateProxy}, nil)
	if got == nil || pmuxerr.ExitCode(got) != 10 || calls.Load() != 0 {
		t.Fatalf("canceled mutation: calls=%d err=%#v", calls.Load(), got)
	}
	for _, name := range []string{"pmux.lock", "journal.jsonl", "audit.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled mutation created %s: %v", name, err)
		}
	}
}

func TestGovernedMutationCancellationAfterDispatchIsInterruptedAndAudited(t *testing.T) {
	root := t.TempDir()
	inner := app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		return app.Result{}, context.Canceled
	})
	governed, err := NewGovernedUseCases(inner, root)
	if err != nil {
		t.Fatal(err)
	}
	_, got := governed.Execute(context.Background(), app.Invocation{Operation: app.OpServiceStop}, nil)
	if got != context.Canceled {
		t.Fatalf("business cancellation identity changed: %#v", got)
	}
	events := readJSONLines(t, filepath.Join(root, "journal.jsonl"))
	if len(events) != 3 || events[2]["state"] != "interrupted" || events[2]["reason"] != governanceCanceled {
		t.Fatalf("journal events = %#v", events)
	}
	entries := readAudit(t, root)
	if len(entries) != 1 || entries[0].Result != audit.ResultFailed || entries[0].ErrorCode != pmuxerr.CodeCanceled {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestGovernanceAuditFailureBlocksDispatchBeforeJournal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "audit.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		calls.Add(1)
		return app.Result{}, nil
	}), root)
	if err != nil {
		t.Fatal(err)
	}
	_, got := governed.Execute(context.Background(), app.Invocation{Operation: app.OpConfigBackup}, nil)
	if got == nil || calls.Load() != 0 {
		t.Fatalf("audit failure dispatched inner: calls=%d err=%#v", calls.Load(), got)
	}
	var typed *pmuxerr.Error
	if !errors.As(got, &typed) || typed.Code != pmuxerr.JournalCorrupt {
		t.Fatalf("audit preparation failure = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "journal.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("audit failure began journal transaction: %v", err)
	}
}

func TestGovernanceJournalFailureBlocksDispatch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "journal.jsonl"), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		calls.Add(1)
		return app.Result{}, nil
	}), root)
	if err != nil {
		t.Fatal(err)
	}
	_, got := governed.Execute(context.Background(), app.Invocation{Operation: app.OpSetup}, nil)
	if got == nil || calls.Load() != 0 {
		t.Fatalf("journal failure dispatched inner: calls=%d err=%#v", calls.Load(), got)
	}
	var typed *pmuxerr.Error
	if !errors.As(got, &typed) || typed.Code != pmuxerr.JournalCorrupt {
		t.Fatalf("journal failure = %#v", got)
	}
	info, err := os.Stat(filepath.Join(root, "audit.jsonl"))
	if err != nil || info.Size() != 0 {
		t.Fatalf("begin failure audit preflight = info %#v err %v", info, err)
	}
}

func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func readAudit(t *testing.T, root string) []audit.Entry {
	t.Helper()
	log, err := audit.New(filepath.Join(root, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := log.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertGovernanceExcludes(t *testing.T, root string, forbidden ...string) {
	t.Helper()
	for _, name := range []string{"journal.jsonl", "audit.jsonl"} {
		payload, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range forbidden {
			if strings.Contains(string(payload), value) {
				t.Fatalf("%s contains forbidden input %q: %s", name, value, payload)
			}
		}
	}
}

func TestStartupRecoversInProgressJournalTransactions(t *testing.T) {
	root := t.TempDir()
	journalPath := filepath.Join(root, "journal.jsonl")
	body := []byte(`{"version":1,"sequence":1,"timestamp":"2026-07-20T00:00:00Z","type":"begin","tx_id":"op_test_pending","operation":"setup","state":"in_progress"}` + "\n" +
		`{"version":1,"sequence":2,"timestamp":"2026-07-20T00:00:01Z","type":"step","tx_id":"op_test_pending","step":{"name":"dispatch","action":"execute","target":"installation","at":"2026-07-20T00:00:01Z"}}` + "\n")
	if err := os.WriteFile(journalPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	governed, err := NewGovernedUseCases(app.UseCaseFunc(func(_ context.Context, _ app.Invocation, _ app.EventSink) (app.Result, error) {
		calls.Add(1)
		return app.Result{}, nil
	}), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := governed.Execute(context.Background(), app.Invocation{Operation: app.OpSetup, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner calls = %d", calls.Load())
	}
	events := readJSONLines(t, journalPath)
	found := false
	for _, event := range events {
		if event["type"] == "state" && event["tx_id"] == "op_test_pending" && event["state"] == "interrupted" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pending transaction was not interrupted: %#v", events)
	}
}
