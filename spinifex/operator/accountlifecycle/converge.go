package accountlifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// ComputeStage is the EC2 compute cleanup and its read-only remainder.
type ComputeStage interface {
	accountcleanup.ComputeCleaner
	accountcleanup.ComputeRemainder
}

// StorageStage is the EC2 storage cleanup and its read-only remainder.
type StorageStage interface {
	accountcleanup.StorageCleaner
	accountcleanup.StorageRemainder
}

var errComputeCleanupMissing = errors.New("teardown needs the EC2 compute and storage cleanup")

// Converge bounds the loop that repeats the compute and storage cleanup inside
// one teardown. Each cleanup call returns without waiting for teardown, so the
// loop is what turns a terminate into a deleted volume.
type Converge struct {
	// Budget is the longest the loop runs, well under the job's own budget.
	Budget time.Duration
	// MinBackoff and MaxBackoff bound the wait between passes, which doubles.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// IncompletePasses is how many passes in a row may end with a read that did
	// not complete before the loop gives up.
	IncompletePasses int
}

// DefaultConverge is sized under the 2 h job budget and over the 10 min
// stuck-terminate backstop, so a wedged terminate still ends the loop clean.
func DefaultConverge() Converge {
	return Converge{Budget: 30 * time.Minute, MinBackoff: 5 * time.Second, MaxBackoff: time.Minute, IncompletePasses: 3}
}

func (e *Engine) convergeConfig() Converge {
	cfg := DefaultConverge()
	if e.Converge.Budget > 0 {
		cfg.Budget = e.Converge.Budget
	}
	if e.Converge.MinBackoff > 0 {
		cfg.MinBackoff = e.Converge.MinBackoff
	}
	if e.Converge.MaxBackoff > 0 {
		cfg.MaxBackoff = e.Converge.MaxBackoff
	}
	if e.Converge.IncompletePasses > 0 {
		cfg.IncompletePasses = e.Converge.IncompletePasses
	}
	return cfg
}

// holdsFence stops a holder that has been superseded or cancelled before it
// issues another terminate or delete. It reads the account back and requires
// it to still be TERMINATING under this operation and fence.
func (e *Engine) holdsFence(ctx context.Context, req Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account, err := e.Accounts.GetAccount(req.AccountID)
	if err != nil {
		return fmt.Errorf("read account to confirm the fence: %w", err)
	}
	op := account.TerminationOp
	if account.Status != AccountStatusTerminating || op == nil || op.OperationID != req.OperationID || op.Fence != req.Fence {
		return errFenceLost
	}
	return nil
}

var errFenceLost = errors.New("the account is no longer held under this operation and fence")

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// convergent reports whether time alone can resolve an item.
func convergent(it accountcleanup.Item) bool {
	if it.Disposition == accountcleanup.DispositionPending {
		return true
	}
	switch it.Reason {
	case accountcleanup.ReasonTerminating, accountcleanup.ReasonVolumeDeleting, accountcleanup.ReasonTeardownUnconfirmed:
		return true
	}
	return false
}

// passSignature identifies what a pass left, so two identical passes in a row
// with nothing convergent show the loop is not going anywhere.
func passSignature(legacy []Stuck, outcomes ...*accountcleanup.Outcome) string {
	var lines []string
	for _, s := range legacy {
		lines = append(lines, "legacy/"+s.Resource.Kind+"/"+s.Resource.ID+"/"+s.Reason)
	}
	for _, out := range outcomes {
		for _, it := range out.Items {
			lines = append(lines, it.Kind+"/"+it.ResourceID+"/"+it.Disposition+"/"+it.Reason+"/"+it.State)
		}
		lines = append(lines, "incomplete:"+strings.Join(out.IncompleteReasons, ","))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// convergeCompute repeats the compute cleanup, the storage-position legacy
// reapers, the storage cleanup and the remainder read until the account holds
// no instance, spot request or volume, or the loop proves it cannot make
// progress. It ends the teardown with the stuck items named, never with a
// shorter list: a lost fence or a cancelled context is an error.
func (e *Engine) convergeCompute(ctx context.Context, req Request, result *Result) error {
	cfg := e.convergeConfig()
	started := time.Now()
	deadline := started.Add(cfg.Budget)
	backoff := cfg.MinBackoff
	wait := e.wait
	if wait == nil {
		wait = waitContext
	}

	attachments := StageResult{Stage: StageAttachments}
	storage := StageResult{Stage: StageStorage}
	var lastSignature string
	var idlePasses, incompletePasses int

	for pass := 1; ; pass++ {
		if err := e.holdsFence(ctx, req); err != nil {
			return err
		}
		computeOut, err := e.Compute.CleanupCompute(ctx, &accountcleanup.ComputeRequest{
			AccountID: req.AccountID, CorrelationRef: req.OperationID, Force: req.Force,
		})
		if err != nil {
			return fmt.Errorf("compute cleanup: %w", err)
		}
		legacy, err := e.runStage(ctx, StageStorage, req)
		if err != nil {
			return err
		}
		storageOut, err := e.Storage.CleanupStorage(ctx, &accountcleanup.StorageRequest{
			AccountID: req.AccountID, CorrelationRef: req.OperationID, Force: req.Force,
		})
		if err != nil {
			return fmt.Errorf("storage cleanup: %w", err)
		}
		computeLeft, err := e.Compute.ComputeRemaining(ctx, req.AccountID)
		if err != nil {
			return fmt.Errorf("compute remainder: %w", err)
		}
		storageLeft, err := e.Storage.StorageRemaining(ctx, req.AccountID)
		if err != nil {
			return fmt.Errorf("storage remainder: %w", err)
		}

		attachments.Deleted = appendDeleted(attachments.Deleted, computeOut)
		storage.Deleted = appendDeleted(storage.Deleted, storageOut)
		for _, r := range legacy.Deleted {
			if !containsResource(storage.Deleted, r) {
				storage.Deleted = append(storage.Deleted, r)
			}
		}
		storage.Stuck = legacy.Stuck
		if e.OnPass != nil {
			e.OnPass(ctx, pass)
		}
		e.logger().InfoContext(ctx, "Teardown convergence pass",
			"account_id", req.AccountID, "pass", pass,
			"compute_remaining", len(computeLeft.Items), "storage_remaining", len(storageLeft.Items),
			"legacy_stuck", len(legacy.Stuck), "elapsed_ms", otelsetup.Millis(time.Since(started)))

		if len(computeLeft.Items) == 0 && len(storageLeft.Items) == 0 &&
			computeLeft.Status == accountcleanup.StatusClean && storageLeft.Status == accountcleanup.StatusClean &&
			len(legacy.Stuck) == 0 {
			return e.finishConverge(ctx, result, attachments, storage, started)
		}

		var moving bool
		for _, out := range []*accountcleanup.Outcome{computeOut, storageOut} {
			moving = moving || slices.ContainsFunc(out.Items, convergent)
		}
		if len(computeOut.IncompleteReasons)+len(storageOut.IncompleteReasons)+len(computeLeft.IncompleteReasons)+len(storageLeft.IncompleteReasons) > 0 {
			incompletePasses++
		} else {
			incompletePasses = 0
		}
		signature := passSignature(legacy.Stuck, computeOut, storageOut, computeLeft, storageLeft)
		if signature == lastSignature && !moving {
			idlePasses++
		} else {
			idlePasses = 0
		}
		lastSignature = signature

		reason := ""
		switch {
		case idlePasses >= 1:
			reason = "only items time cannot resolve remain"
		case incompletePasses >= cfg.IncompletePasses:
			reason = "a read stayed incomplete"
		case time.Now().Add(backoff).After(deadline):
			reason = "the convergence budget ended"
		}
		if reason != "" {
			attachments.Stuck = stuckFrom(attachments.Stuck[:0], computeOut, computeLeft)
			storage.Stuck = append(legacy.Stuck, stuckFrom(nil, storageOut, storageLeft)...)
			if err := e.finishConverge(ctx, result, attachments, storage, started); err != nil {
				return err
			}
			e.logger().WarnContext(ctx, "Account teardown stopped converging",
				"accountID", req.AccountID, "reason", reason, "passes", pass)
			return fmt.Errorf("%d resources left (%s): %w", result.StuckCount(), reason, ErrResourcesStuck)
		}

		if err := wait(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, cfg.MaxBackoff)
	}
}

func (e *Engine) finishConverge(ctx context.Context, result *Result, attachments, storage StageResult, started time.Time) error {
	elapsed := time.Since(started).String()
	attachments.Elapsed, storage.Elapsed = elapsed, elapsed
	for _, stage := range []StageResult{attachments, storage} {
		result.Stages = append(result.Stages, stage)
		if e.OnStage != nil {
			e.OnStage(ctx, stage)
		}
	}
	return nil
}

func appendDeleted(into []Resource, out *accountcleanup.Outcome) []Resource {
	for _, it := range out.Items {
		if it.Disposition == accountcleanup.DispositionDeleted {
			r := Resource{Kind: it.Kind, ID: it.ResourceID}
			if !containsResource(into, r) {
				into = append(into, r)
			}
		}
	}
	return into
}

// stuckFrom names every item the account still holds with the reason the
// cleanup gave for keeping it, or that the record remains. An incomplete read
// is named too, because it proves nothing about what is left.
func stuckFrom(into []Stuck, cleanup, remainder *accountcleanup.Outcome) []Stuck {
	reasons := map[string]string{}
	for _, it := range cleanup.Items {
		if it.Reason != "" && it.Disposition != accountcleanup.DispositionDeleted {
			reasons[it.Kind+"/"+it.ResourceID] = it.Reason
		}
	}
	for _, it := range remainder.Items {
		reason := reasons[it.Kind+"/"+it.ResourceID]
		if reason == "" {
			reason = it.Reason
		}
		into = append(into, Stuck{Resource: Resource{Kind: it.Kind, ID: it.ResourceID, Detail: it.State}, Reason: reason})
	}
	for _, reason := range slices.Concat(cleanup.IncompleteReasons, remainder.IncompleteReasons) {
		into = append(into, Stuck{Resource: Resource{Kind: "inventory", ID: reason}, Reason: reason})
	}
	return into
}

// rereadComputeStorage reads the instances, spot requests and volumes the
// account still holds immediately before the record is removed, so anything
// that appeared after the convergence loop ended keeps the account. A read
// that fails or is incomplete is named, never taken as empty.
func (e *Engine) rereadComputeStorage(ctx context.Context, accountID string, result *Result) {
	compute, cerr := e.Compute.ComputeRemaining(ctx, accountID)
	storage, serr := e.Storage.StorageRemaining(ctx, accountID)
	e.addFinalStuck(result, StageAttachments, accountID, compute, cerr)
	e.addFinalStuck(result, StageStorage, accountID, storage, serr)
}

func (e *Engine) addFinalStuck(result *Result, stage Stage, accountID string, out *accountcleanup.Outcome, err error) {
	var stuck []Stuck
	if err != nil || out == nil {
		stuck = []Stuck{{Resource: Resource{Kind: "inventory", ID: stage.String()}, Reason: operatorv1.AccountingReasonRereadIncomplete}}
	} else {
		stuck = stuckFrom(nil, &accountcleanup.Outcome{}, out)
	}
	if len(stuck) == 0 {
		return
	}
	for i := range result.Stages {
		if result.Stages[i].Stage == stage {
			result.Stages[i].Stuck = append(result.Stages[i].Stuck, stuck...)
			return
		}
	}
	result.Stages = append(result.Stages, StageResult{Stage: stage, Elapsed: "0s", Stuck: stuck})
}
