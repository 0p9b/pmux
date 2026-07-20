package doctor

import (
	"context"
	"fmt"

	domaindoctor "github.com/0p9b/pmux/internal/domain/doctor"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

// RollbackFix is required for every mutating fix. Rollback must be idempotent
// and restore the pre-Apply state when Apply changed anything, including when
// Apply returned an error after its commit point.
type RollbackFix interface {
	domaindoctor.Fix
	Rollback(context.Context) error
}

type FixPlan struct {
	CheckIDs []string                 `json:"check_ids"`
	Preview  []domaindoctor.FixResult `json:"preview"`
}

type ConfirmFunc func(context.Context, FixPlan) (bool, error)

type FixOptions struct {
	DryRun  bool
	Yes     bool
	Confirm ConfirmFunc
}

type FixReport struct {
	Plan       FixPlan                  `json:"plan"`
	Results    []domaindoctor.FixResult `json:"results"`
	RolledBack bool                     `json:"rolled_back"`
	Report     Report                   `json:"report"`
}

type FixRunner struct {
	Registry     *Registry
	KnownSecrets []string
}

func (r FixRunner) Run(ctx context.Context, checkIDs []string, opts FixOptions) (FixReport, error) {
	if r.Registry == nil {
		return FixReport{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor registry is required")
	}
	ordered, err := r.selectFixes(ctx, checkIDs)
	if err != nil {
		return FixReport{}, err
	}
	out := FixReport{Plan: FixPlan{CheckIDs: make([]string, 0, len(ordered)), Preview: make([]domaindoctor.FixResult, 0, len(ordered))}}
	if len(ordered) == 0 {
		out.Report, err = Runner(r).Run(ctx, checkIDs...)
		return out, err
	}
	for _, item := range ordered {
		preview, applyErr := item.fix.Apply(ctx, true)
		if applyErr != nil {
			return out, pmuxerr.Wrap(applyErr, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "doctor fix preview failed")
		}
		preview.CheckID = item.check.ID()
		preview = normalizeFixResult(preview, r.KnownSecrets...)
		preview.Changed = false
		preview.Verified = false
		out.Plan.CheckIDs = append(out.Plan.CheckIDs, item.check.ID())
		out.Plan.Preview = append(out.Plan.Preview, preview)
	}
	if opts.DryRun {
		out.Report, err = Runner(r).Run(ctx, out.Plan.CheckIDs...)
		return out, err
	}
	if !opts.Yes {
		if opts.Confirm == nil {
			return out, &pmuxerr.Error{Code: pmuxerr.CodeUsage, Class: pmuxerr.User, Message: "doctor fixes require confirmation", Explanation: "noninteractive operation requires --yes; no changes were made"}
		}
		confirmed, confirmErr := opts.Confirm(ctx, out.Plan)
		if confirmErr != nil {
			return out, pmuxerr.Wrap(confirmErr, pmuxerr.CodeCanceled, pmuxerr.User, "doctor fix confirmation failed")
		}
		if !confirmed {
			return out, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "doctor fixes were canceled; no changes were made")
		}
	}

	applied := make([]fixItem, 0, len(ordered))
	for _, item := range ordered {
		if err := ctx.Err(); err != nil {
			return r.rollback(ctx, out, applied, pmuxerr.Wrap(err, pmuxerr.CodeInterrupted, pmuxerr.Environment, "doctor fix was interrupted"))
		}
		current, currentErr := r.observedCheck(ctx, item.check.ID())
		if currentErr != nil {
			return r.rollback(ctx, out, applied, currentErr)
		}
		if current.Status != domaindoctor.StatusFail {
			continue
		}
		fixResult, applyErr := item.fix.Apply(ctx, false)
		fixResult.CheckID = item.check.ID()
		fixResult = normalizeFixResult(fixResult, r.KnownSecrets...)
		out.Results = append(out.Results, fixResult)
		// Include the current fix in rollback even when Apply failed: it may have
		// crossed its commit point before returning the error.
		applied = append(applied, item)
		if applyErr != nil {
			return r.rollback(ctx, out, applied, pmuxerr.Wrap(applyErr, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "doctor fix failed"))
		}
		observed, observedErr := r.observedCheck(ctx, item.check.ID())
		if observedErr != nil {
			return r.rollback(ctx, out, applied, observedErr)
		}
		if !fixResult.Verified || observed.Status != domaindoctor.StatusPass {
			cause := fmt.Errorf("fix %s did not pass verification: status=%s verified=%t", item.fix.ID(), observed.Status, fixResult.Verified)
			return r.rollback(ctx, out, applied, pmuxerr.Wrap(cause, pmuxerr.ServiceHealthDeadline, pmuxerr.Environment, "doctor fix did not resolve its check"))
		}
	}
	out.Report, err = Runner(r).Run(ctx)
	if err != nil {
		return out, err
	}
	return out, nil
}

type fixItem struct {
	check domaindoctor.Check
	fix   RollbackFix
}

func (r FixRunner) selectFixes(ctx context.Context, ids []string) ([]fixItem, error) {
	requested := make(map[string]bool)
	for _, id := range ids {
		requested[id] = true
	}
	report, err := Runner(r).Run(ctx, ids...)
	if err != nil {
		return nil, err
	}
	observed := make(map[string]domaindoctor.CheckResult, len(report.Checks))
	for _, checkResult := range report.Checks {
		observed[checkResult.ID] = checkResult
	}
	items := make([]fixItem, 0)
	for _, check := range r.Registry.Checks() {
		if len(requested) > 0 && !requested[check.ID()] {
			continue
		}
		if observed[check.ID()].Status != domaindoctor.StatusFail {
			continue
		}
		fix, ok := r.Registry.Fix(check.ID())
		if !ok {
			continue
		}
		rollback, ok := fix.(RollbackFix)
		if !ok {
			return nil, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "mutating doctor fix does not implement rollback")
		}
		items = append(items, fixItem{check: check, fix: rollback})
	}
	return items, nil
}

func (r FixRunner) observedCheck(ctx context.Context, id string) (domaindoctor.CheckResult, error) {
	report, err := Runner(r).Run(ctx, id)
	if err != nil {
		return domaindoctor.CheckResult{}, err
	}
	for _, result := range report.Checks {
		if result.ID == id {
			return result, nil
		}
	}
	return domaindoctor.CheckResult{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor verification result is missing")
}

func normalizeFixResult(result domaindoctor.FixResult, secrets ...string) domaindoctor.FixResult {
	result.Summary = redact.Known(safeText(result.Summary), secrets...)
	return result
}

func (r FixRunner) rollback(ctx context.Context, out FixReport, applied []fixItem, cause error) (FixReport, error) {
	out.RolledBack = true
	var rollbackErr error
	// Do not inherit a canceled request for rollback; rollback is a bounded
	// recovery action and must still be attempted after cancellation.
	rollbackCtx := context.WithoutCancel(ctx)
	for i := len(applied) - 1; i >= 0; i-- {
		if err := applied[i].fix.Rollback(rollbackCtx); err != nil && rollbackErr == nil {
			rollbackErr = err
		}
	}
	if rollbackErr != nil {
		return out, &pmuxerr.Error{Code: pmuxerr.JournalCorrupt, Class: pmuxerr.Internal, Message: "doctor fix failed and rollback could not restore the prior state", Evidence: []string{"rollback returned an error; use --verbose for the wrapped cause"}, Cause: cause}
	}
	return out, cause
}
