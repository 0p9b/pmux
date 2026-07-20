package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	adapterlock "github.com/0p9b/pmux/internal/adapter/lock"
	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/audit"
	domainjournal "github.com/0p9b/pmux/internal/domain/journal"
	journalstore "github.com/0p9b/pmux/internal/journal"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	governanceActor             = "local"
	governanceDispatch          = "dispatch"
	governanceExecute           = "execute"
	governanceFailed            = "operation failed"
	governanceCanceled          = "operation canceled"
	governanceRecordError       = "dispatch record failed"
	governanceCommitError       = "completion record failed"
	governanceStartupRecovery   = "startup-recovery"
	governanceInterruptedReason = "interrupted mutation recovered at process startup"
)

type operationJournal interface {
	Begin(string, map[string]string) (domainjournal.TxID, error)
	Record(domainjournal.TxID, domainjournal.Step) error
	Commit(domainjournal.TxID) error
	Fail(domainjournal.TxID, string) error
	Interrupt(domainjournal.TxID, string) error
	Pending() ([]domainjournal.Tx, error)
}

type auditLog interface {
	Prepare() error
	Append(audit.Entry) error
}

// GovernedUseCases decorates the application router with the single-host
// mutation lock, durable operation journal, and redacted audit log. It never
// inspects or serializes invocation arguments or options.
type GovernedUseCases struct {
	inner   app.UseCases
	lock    *adapterlock.Manager
	journal operationJournal
	audit   auditLog
}

var _ app.UseCases = (*GovernedUseCases)(nil)

// NewGovernedUseCases constructs governance at the canonical paths below the
// state root, then recovers any crash-leaked journal transactions under the
// advisory mutation lock before accepting commands.
func NewGovernedUseCases(inner app.UseCases, stateRoot string) (app.UseCases, error) {
	if inner == nil {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Internal, "governed use cases require an application router")
	}
	manager, err := adapterlock.New(filepath.Join(stateRoot, "pmux.lock"))
	if err != nil {
		return nil, err
	}
	operations, err := journalstore.New(filepath.Join(stateRoot, "journal.jsonl"))
	if err != nil {
		return nil, err
	}
	actions, err := audit.New(filepath.Join(stateRoot, "audit.jsonl"))
	if err != nil {
		return nil, err
	}
	governed := &GovernedUseCases{inner: inner, lock: manager, journal: operations, audit: actions}
	if _, statErr := os.Stat(filepath.Join(stateRoot, "journal.jsonl")); statErr == nil {
		// Best-effort recovery of crash-leaked in-progress transactions. A corrupt
		// journal is left for the first mutating command, which already fails closed.
		_ = governed.recoverInterruptedPending(context.Background())
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, governancePersistence(statErr, "PMux could not inspect the operation journal.")
	}
	return governed, nil
}

// IsMutation is the pure classification shared by CLI and TUI invocations.
// Conditional operations are mutations only when their mutating option is
// present; options are inspected only for classification and never persisted.
func IsMutation(in app.Invocation) bool {
	switch in.Operation {
	case app.OpSetup,
		app.OpProvidersLogin, app.OpProvidersEnable, app.OpProvidersDisable, app.OpProvidersRemove,
		app.OpModelsFavorite, app.OpModelsUnfavorite,
		app.OpServiceStart, app.OpServiceStop, app.OpServiceRestart, app.OpServiceInstall, app.OpServiceUninstall,
		app.OpConfigSet, app.OpConfigEdit, app.OpConfigBackup, app.OpConfigRestore,
		app.OpUpdateSelf, app.OpUpdateProxy:
		return true
	case app.OpDoctor:
		return invocationBool(in.Options, "fix") || strings.TrimSpace(invocationString(in.Options, "bundle")) != ""
	case app.OpServiceLogs:
		return strings.TrimSpace(invocationString(in.Options, "clear")) != "" || strings.TrimSpace(invocationString(in.Options, "output")) != ""
	default:
		return false
	}
}

func (g *GovernedUseCases) recoverInterruptedPending(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return canceledGovernance(err)
	}
	handle, err := g.lock.TryAcquire(governanceStartupRecovery)
	if err != nil {
		var busy *adapterlock.BusyError
		if errors.As(err, &busy) {
			// Another live PMux mutation owns recovery; leave pending state alone.
			return nil
		}
		return err
	}
	defer func() { _ = handle.Release() }()

	pending, err := g.journal.Pending()
	if err != nil {
		return governancePersistence(err, "PMux could not inspect interrupted operation journal state.")
	}
	for _, tx := range pending {
		if tx.State != journalstore.StateInProgress {
			// Failed/interrupted history remains visible for doctor/manual guidance.
			continue
		}
		// Generic dispatch-only transactions have no adapter undo payload; mark
		// them interrupted so a later mutation can proceed safely.
		if err := g.journal.Interrupt(tx.ID, governanceInterruptedReason); err != nil {
			return governancePersistence(err, "PMux could not recover an interrupted mutation journal transaction.")
		}
		if err := g.appendAudit(tx.ID, tx.Operation, mutationTarget(app.Operation(tx.Operation)), audit.ResultFailed, pmuxerr.CodeInterrupted); err != nil {
			return governancePersistence(err, "PMux could not audit interrupted-mutation recovery.")
		}
	}
	return nil
}

func (g *GovernedUseCases) Execute(ctx context.Context, in app.Invocation, sink app.EventSink) (result app.Result, retErr error) {
	if !IsMutation(in) {
		return g.inner.Execute(ctx, in, sink)
	}
	if err := ctx.Err(); err != nil {
		return app.Result{}, canceledGovernance(err)
	}

	operation := string(in.Operation)
	handle, err := g.lock.TryAcquire(operation)
	if err != nil {
		return app.Result{}, err
	}
	defer func() {
		if err := handle.Release(); err != nil && retErr == nil {
			retErr = pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "PMux could not release the mutation lock.")
		}
	}()

	if err := g.audit.Prepare(); err != nil {
		return app.Result{}, governancePersistence(err, "PMux could not prepare the operation audit log; no mutation was attempted.")
	}

	tx, err := g.journal.Begin(operation, nil)
	if err != nil {
		return app.Result{}, governancePersistence(err, "PMux could not begin the operation journal transaction.")
	}
	target := mutationTarget(in.Operation)
	if err := g.journal.Record(tx, domainjournal.Step{Name: governanceDispatch, Action: governanceExecute, Target: target}); err != nil {
		return app.Result{}, g.failBeforeDispatch(tx, operation, target, err)
	}
	if err := ctx.Err(); err != nil {
		canceled := canceledGovernance(err)
		return app.Result{}, g.finishFailure(tx, operation, target, canceled, true)
	}

	result, businessErr := g.inner.Execute(ctx, in, sink)
	if businessErr != nil {
		return result, g.finishFailure(tx, operation, target, businessErr, isCancellation(businessErr))
	}
	if result.ExitCode != 0 {
		if err := g.finishResultFailure(tx, operation, target, result.ExitCode); err != nil {
			return result, err
		}
		return result, nil
	}
	if err := g.journal.Commit(tx); err != nil {
		terminalErr := g.journal.Fail(tx, governanceCommitError)
		auditErr := g.appendAudit(tx, operation, target, audit.ResultFailed, canonicalErrorCode(err))
		if terminalErr != nil {
			return result, governancePersistence(errors.Join(err, terminalErr, auditErr), "PMux could not persist the operation completion state.")
		}
		if auditErr != nil {
			return result, governancePersistence(errors.Join(err, auditErr), "PMux could not persist the operation audit record.")
		}
		return result, governancePersistence(err, "PMux could not commit the operation journal transaction.")
	}
	if err := g.appendAudit(tx, operation, target, audit.ResultOK, ""); err != nil {
		return result, governancePersistence(err, "PMux could not persist the operation audit record.")
	}
	return result, nil
}

func (g *GovernedUseCases) failBeforeDispatch(tx domainjournal.TxID, operation, target string, cause error) error {
	terminalErr := g.journal.Fail(tx, governanceRecordError)
	auditErr := g.appendAudit(tx, operation, target, audit.ResultFailed, canonicalErrorCode(cause))
	if terminalErr != nil || auditErr != nil {
		return governancePersistence(errors.Join(cause, terminalErr, auditErr), "PMux governance failed before command dispatch.")
	}
	return governancePersistence(cause, "PMux could not record command dispatch; no mutation was attempted.")
}

func (g *GovernedUseCases) finishFailure(tx domainjournal.TxID, operation, target string, businessErr error, canceled bool) error {
	if err := g.persistFailure(tx, operation, target, canonicalErrorCode(businessErr), canceled); err != nil {
		return err
	}
	return businessErr
}

func (g *GovernedUseCases) finishResultFailure(tx domainjournal.TxID, operation, target string, exitCode int) error {
	code := canonicalExitCode(exitCode)
	return g.persistFailure(tx, operation, target, code, code == pmuxerr.CodeCanceled || code == pmuxerr.CodeInterrupted)
}

func (g *GovernedUseCases) persistFailure(tx domainjournal.TxID, operation, target, code string, canceled bool) error {
	var terminalErr error
	if canceled {
		terminalErr = g.journal.Interrupt(tx, governanceCanceled)
	} else {
		terminalErr = g.journal.Fail(tx, governanceFailed)
	}
	auditErr := g.appendAudit(tx, operation, target, audit.ResultFailed, code)
	if terminalErr != nil || auditErr != nil {
		return governancePersistence(errors.Join(terminalErr, auditErr), "PMux could not persist governance records for the failed operation.")
	}
	return nil
}

func (g *GovernedUseCases) appendAudit(tx domainjournal.TxID, operation, target string, result audit.Result, code string) error {
	return g.audit.Append(audit.Entry{
		Operation: string(tx),
		Actor:     governanceActor,
		Command:   operation,
		Target:    target,
		Result:    result,
		ErrorCode: code,
	})
}

func mutationTarget(operation app.Operation) string {
	switch operation {
	case app.OpSetup:
		return "installation"
	case app.OpProvidersLogin, app.OpProvidersEnable, app.OpProvidersDisable, app.OpProvidersRemove:
		return "provider"
	case app.OpModelsFavorite, app.OpModelsUnfavorite:
		return "model-preferences"
	case app.OpDoctor:
		return "diagnostics"
	case app.OpServiceStart, app.OpServiceStop, app.OpServiceRestart, app.OpServiceInstall, app.OpServiceUninstall, app.OpServiceLogs:
		return "service"
	case app.OpConfigSet, app.OpConfigEdit, app.OpConfigBackup, app.OpConfigRestore:
		return "configuration"
	case app.OpUpdateSelf, app.OpUpdateProxy:
		return "managed-update"
	default:
		return "local-state"
	}
}

func invocationBool(options map[string]any, key string) bool {
	if options == nil {
		return false
	}
	value, _ := options[key].(bool)
	return value
}

func invocationString(options map[string]any, key string) string {
	if options == nil {
		return ""
	}
	value, _ := options[key].(string)
	return value
}

func canonicalErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var typed *pmuxerr.Error
	if errors.As(err, &typed) && typed.Code != "" {
		return typed.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return pmuxerr.CodeCanceled
	}
	return pmuxerr.UnhandledInternal
}

func canonicalExitCode(code int) string {
	switch code {
	case 0:
		return ""
	case 2:
		return pmuxerr.CodeUsage
	case 3:
		return pmuxerr.CodeConfig
	case 4:
		return pmuxerr.CodeDependencyMissing
	case 5:
		return pmuxerr.CodeAuth
	case 6:
		return pmuxerr.CodeNetwork
	case 7:
		return pmuxerr.CodeUnhealthy
	case 9:
		return pmuxerr.CodeOwnershipConflict
	case 10:
		return pmuxerr.CodeCanceled
	case 125:
		return pmuxerr.CodeLaunchFailed
	case 126:
		return pmuxerr.CodeNotExecutable
	case 127:
		return pmuxerr.CodeExecutableMissing
	case 130:
		return pmuxerr.CodeInterrupted
	default:
		return pmuxerr.CodeInternal
	}
}

func isCancellation(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var typed *pmuxerr.Error
	return errors.As(err, &typed) && (typed.Code == pmuxerr.CodeCanceled || typed.Code == pmuxerr.CodeInterrupted)
}

func canceledGovernance(cause error) error {
	return pmuxerr.Wrap(cause, pmuxerr.CodeCanceled, pmuxerr.User, "Operation was canceled before command dispatch.")
}

func governancePersistence(cause error, message string) error {
	return pmuxerr.Wrap(cause, pmuxerr.JournalCorrupt, pmuxerr.Internal, message)
}
