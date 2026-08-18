package accountteardown

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// AccountStatusTerminating mirrors handlers_iam.AccountStatusTerminating.
// Redeclared so this package does not depend on the IAM service to be tested.
const AccountStatusTerminating = "TERMINATING"

// Engine tears an account down. Reapers are consulted in the order given
// within a stage, so a caller registering snapshots before volumes gets
// snapshots deleted first — which is required, because a volume with a live
// snapshot refuses to delete.
type Engine struct {
	Accounts AccountStore
	Reapers  []Reaper
	Timeouts Timeouts
	Logger   *slog.Logger

	// OnStage reports each finished stage as it happens, so a caller running a
	// teardown in the background can publish progress. A teardown takes minutes
	// and this is the only view into it while it runs.
	OnStage func(ctx context.Context, stage StageResult)
}

// NewEngine builds an engine with the default timeouts.
func NewEngine(accounts AccountStore, reapers ...Reaper) *Engine {
	return &Engine{
		Accounts: accounts,
		Reapers:  reapers,
		Timeouts: DefaultTimeouts(),
		Logger:   slog.Default(),
	}
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// Precheck applies the rules that decide whether a teardown may start at all,
// without listing anything. A caller that runs the teardown in the background
// needs these answered synchronously: a protected account, an unknown id or a
// mistyped name confirmation must fail the request, not the job.
func (e *Engine) Precheck(accountID, accountName string) error {
	_, err := e.checkDeletable(accountID, accountName)
	return err
}

// Inventory reports what a teardown would remove, grouped by stage, without
// deleting anything or changing the account's status.
func (e *Engine) Inventory(ctx context.Context, accountID string) (*Result, error) {
	account, err := e.checkDeletable(accountID, "")
	if err != nil {
		return nil, err
	}

	result := &Result{
		AccountID:   account.AccountID,
		AccountName: account.AccountName,
		DryRun:      true,
	}
	for _, stage := range Stages() {
		stageResult := StageResult{Stage: stage, Elapsed: "0s"}
		for _, reaper := range e.reapersFor(stage) {
			found, err := reaper.List(ctx, accountID)
			if err != nil {
				return nil, fmt.Errorf("inventory %s: %w", reaper.Kind(), err)
			}
			stageResult.Deleted = append(stageResult.Deleted, found...)
		}
		result.Stages = append(result.Stages, stageResult)
	}
	return result, nil
}

// Teardown empties the account and then removes it.
//
// The account is marked TERMINATING before anything is deleted. Without that
// the customer can keep creating resources behind the cascade and it never
// converges; with it, a stage that has drained stays drained.
func (e *Engine) Teardown(ctx context.Context, req Request) (*Result, error) {
	account, err := e.checkDeletable(req.AccountID, req.AccountName)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return e.Inventory(ctx, req.AccountID)
	}

	if _, err := e.Accounts.SetAccountStatus(req.AccountID, AccountStatusTerminating); err != nil {
		return nil, fmt.Errorf("mark account terminating: %w", err)
	}
	e.logger().InfoContext(ctx, "Account teardown started",
		"accountID", account.AccountID, "accountName", account.AccountName, "force", req.Force)

	result := &Result{
		AccountID:   account.AccountID,
		AccountName: account.AccountName,
		Forced:      req.Force,
	}
	for _, stage := range Stages() {
		stageResult, err := e.runStage(ctx, stage, req)
		if err != nil {
			return result, err
		}
		result.Stages = append(result.Stages, stageResult)
		if e.OnStage != nil {
			e.OnStage(ctx, stageResult)
		}
	}

	if stuck := result.StuckCount(); stuck > 0 {
		// The account stays TERMINATING on purpose. Removing the record now
		// would leave the residue with no owner, and an orphan nobody can
		// attribute is worse than an account that visibly failed to empty.
		e.logger().ErrorContext(ctx, "Account teardown incomplete — account record kept",
			"accountID", account.AccountID, "stuck", stuck)
		return result, fmt.Errorf("%d resources left: %w", stuck, ErrResourcesStuck)
	}

	if err := e.Accounts.DeleteAccount(req.AccountID); err != nil {
		return result, fmt.Errorf("delete account record: %w", err)
	}
	result.AccountDeleted = true
	e.logger().InfoContext(ctx, "Account teardown complete",
		"accountID", account.AccountID, "deleted", result.DeletedCount())
	return result, nil
}

// runStage deletes everything in one stage and then waits for it to actually
// drain. Deletes here are mostly asynchronous — a terminate returns long
// before the guest is gone — so a stage is finished when a fresh listing comes
// back empty, not when the delete calls returned.
func (e *Engine) runStage(ctx context.Context, stage Stage, req Request) (StageResult, error) {
	started := time.Now()
	result := StageResult{Stage: stage}
	reapers := e.reapersFor(stage)
	if len(reapers) == 0 {
		result.Elapsed = time.Since(started).String()
		return result, nil
	}

	deadline := started.Add(e.stageDrain())
	// reasons keeps the last failure per resource so a resource that is still
	// listed at the deadline is reported with why it would not go, rather than
	// only that it is still there.
	reasons := map[string]string{}

	for {
		remaining, err := e.sweep(ctx, reapers, req, &result, reasons)
		if err != nil {
			return result, err
		}
		if len(remaining) == 0 {
			result.Elapsed = time.Since(started).String()
			return result, nil
		}
		if !time.Now().Before(deadline) {
			for _, resource := range remaining {
				reason := reasons[resource.Kind+"/"+resource.ID]
				if reason == "" {
					reason = "still present after the stage drain timeout"
				}
				result.Stuck = append(result.Stuck, Stuck{Resource: resource, Reason: reason})
			}
			result.Elapsed = time.Since(started).String()
			e.logger().WarnContext(ctx, "Teardown stage did not drain",
				"accountID", req.AccountID, "stage", stage.String(), "stuck", len(result.Stuck))
			return result, nil
		}

		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(e.drainPoll()):
		}
	}
}

// sweep lists every reaper in the stage and issues a delete for whatever it
// finds, returning what was still listed at the start of this pass.
func (e *Engine) sweep(
	ctx context.Context,
	reapers []Reaper,
	req Request,
	result *StageResult,
	reasons map[string]string,
) ([]Resource, error) {
	var remaining []Resource
	for _, reaper := range reapers {
		found, err := reaper.List(ctx, req.AccountID)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", reaper.Kind(), err)
		}
		remaining = append(remaining, found...)

		for _, resource := range found {
			if err := reaper.Delete(ctx, req.AccountID, resource, req.Force); err != nil {
				reasons[resource.Kind+"/"+resource.ID] = err.Error()
				e.logger().WarnContext(ctx, "Teardown delete failed",
					"accountID", req.AccountID, "kind", resource.Kind, "id", resource.ID, "err", err)
				continue
			}
			delete(reasons, resource.Kind+"/"+resource.ID)
			if !containsResource(result.Deleted, resource) {
				result.Deleted = append(result.Deleted, resource)
				e.logger().InfoContext(ctx, "Teardown deleted resource",
					"accountID", req.AccountID, "kind", resource.Kind, "id", resource.ID)
			}
		}
	}
	return remaining, nil
}

// checkDeletable applies the two rules that no caller may skip: the protected
// accounts, and the name confirmation. An empty wantName skips the second,
// which is what makes Inventory usable as a read-only probe.
func (e *Engine) checkDeletable(accountID, wantName string) (*Account, error) {
	if reason, protected := protectedAccountIDs[accountID]; protected {
		return nil, fmt.Errorf("%s is the %s account: %w", accountID, reason, ErrProtectedAccount)
	}
	if e.Accounts == nil {
		return nil, errors.New("teardown engine has no account store")
	}

	account, err := e.Accounts.GetAccount(accountID)
	if err != nil {
		return nil, fmt.Errorf("read account %s: %w", accountID, err)
	}
	if wantName != "" && account.AccountName != wantName {
		// Never echo the stored name: a caller who guessed the id would learn
		// the tenant's email address by probing this.
		return nil, fmt.Errorf("%q does not name account %s: %w", wantName, accountID, ErrAccountNameMismatch)
	}
	return account, nil
}

// reapersFor returns the stage's reapers in registration order.
func (e *Engine) reapersFor(stage Stage) []Reaper {
	var stageReapers []Reaper
	for _, reaper := range e.Reapers {
		if reaper.Stage() == stage {
			stageReapers = append(stageReapers, reaper)
		}
	}
	return stageReapers
}

func (e *Engine) stageDrain() time.Duration {
	if e.Timeouts.StageDrain > 0 {
		return e.Timeouts.StageDrain
	}
	return DefaultTimeouts().StageDrain
}

func (e *Engine) drainPoll() time.Duration {
	if e.Timeouts.DrainPoll > 0 {
		return e.Timeouts.DrainPoll
	}
	return DefaultTimeouts().DrainPoll
}

func containsResource(resources []Resource, want Resource) bool {
	for _, resource := range resources {
		if resource.Kind == want.Kind && resource.ID == want.ID {
			return true
		}
	}
	return false
}

// SortReapers orders reapers by stage while preserving the caller's order
// within each stage, which is where the snapshots-before-volumes dependency
// lives.
func SortReapers(reapers []Reaper) {
	sort.SliceStable(reapers, func(i, j int) bool { return reapers[i].Stage() < reapers[j].Stage() })
}
