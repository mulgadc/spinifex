package accountlifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
)

// ec2PlatformInventoryBudget caps one wait for EC2's platform inventory, never
// past the caller's deadline. A later answer is scan-incomplete.
const ec2PlatformInventoryBudget = 10 * time.Second

var errPlatformCleanupMissing = errors.New("teardown needs the EC2 platform cleanup and inventory")

// platformRun is what the ordinary teardown's platform step did.
type platformRun struct {
	deleted []Resource
	cleanup *operatorv1.PlatformCleanup
}

func (e *Engine) platformBudget() time.Duration {
	if e.ec2PlatformBudget > 0 {
		return e.ec2PlatformBudget
	}
	return ec2PlatformInventoryBudget
}

// awaitPlatformInventory bounds the inventory call by the budget even if the
// implementation ignores its context, and distrusts the reply's shape.
func (e *Engine) awaitPlatformInventory(ctx context.Context, accountID, correlationRef string) ec2v1.PlatformInventoryReply {
	ctx, cancel := context.WithTimeout(ctx, e.platformBudget())
	defer cancel()

	replies := make(chan ec2v1.PlatformInventoryReply, 1)
	go func() { replies <- e.ec2Inventory.PlatformInventory(ctx, accountID, correlationRef) }()
	select {
	case reply := <-replies:
		return normalisePlatformInventory(reply, accountID, correlationRef)
	case <-ctx.Done():
		return ec2v1.IncompletePlatformInventory(accountID, correlationRef, ec2v1.PlatformReasonTimeout)
	}
}

// listPlatformKinds is what EC2's inventory says the account holds, for the
// dry run. A kind whose scan was incomplete cannot be listed, so it is an error.
func (e *Engine) listPlatformKinds(ctx context.Context, accountID string) ([]Resource, error) {
	if e.ec2Inventory == nil {
		return nil, nil
	}
	reply := e.awaitPlatformInventory(ctx, accountID, "")
	var found []Resource
	for _, kind := range ec2v1.PlatformKinds() {
		if _, complete := kindInventory(&reply, kind); !complete {
			return nil, fmt.Errorf("inventory %s: scan incomplete", kind)
		}
	}
	for _, item := range reply.Items {
		found = append(found, Resource{Kind: item.Kind, ID: item.ResourceID, Detail: item.Disposition})
	}
	for _, count := range reply.Counts {
		if count.Count > 0 {
			found = append(found, Resource{Kind: count.Kind, ID: count.Class, Detail: strconv.Itoa(count.Count) + " records"})
		}
	}
	return found, nil
}

// cleanPlatform evaluates quiescence again under the same fence, records it, and
// cleans only the kinds that evaluation quiesced. A run that quiesced none cleans
// nothing; the final re-read then decides whether the account can go.
func (e *Engine) cleanPlatform(ctx context.Context, req Request, generation uint64) (*platformRun, error) {
	run := &platformRun{}
	quiescence := e.evaluateQuiescence(ctx, req, generation, true)
	if e.OnQuiesced != nil {
		e.OnQuiesced(ctx, quiescence)
	}
	if len(quiescence.QuiescedScope) == 0 {
		e.logger().WarnContext(ctx, "Account teardown: no L09 kind quiesced, platform cleanup skipped", "accountID", req.AccountID)
		return run, nil
	}
	cleaned, err := e.Cleanup.CleanupPlatform(ctx, &accountcleanup.Request{
		AccountID: req.AccountID, CorrelationRef: req.OperationID,
		Kinds: quiescence.QuiescedScope, DeleteLiveTemplates: true,
	})
	if err != nil {
		return nil, fmt.Errorf("platform cleanup: %w", err)
	}
	run.cleanup = platformCleanup(cleaned)
	for _, item := range run.cleanup.Items {
		if item.Disposition == accountcleanup.DispositionDeleted {
			run.deleted = append(run.deleted, Resource{Kind: item.Kind, ID: item.ResourceID})
		}
	}
	return run, nil
}

// rereadPlatform is the last look before the account record goes. Any record
// still held, or a re-read that could not complete, is a stuck item: the account
// stays TERMINATING and a rerun of the operation converges. Only IDs, classes
// and fixed reasons are recorded.
func (e *Engine) rereadPlatform(ctx context.Context, req Request, run *platformRun, result *Result) {
	reply := e.awaitPlatformInventory(ctx, req.AccountID, req.OperationID)
	var stuck []Stuck
	for _, kind := range ec2v1.PlatformKinds() {
		status, complete := kindInventory(&reply, kind)
		if !complete {
			stuck = append(stuck, Stuck{Resource: Resource{Kind: kind, ID: "inventory"}, Reason: operatorv1.AccountingReasonRereadIncomplete})
			continue
		}
		if status != ec2v1.PlatformStatusRecordsFound {
			continue
		}
		for _, item := range reply.Items {
			if item.Kind == kind {
				stuck = append(stuck, Stuck{Resource: Resource{Kind: kind, ID: item.ResourceID}, Reason: remainderReason(run, item.ResourceID, kind)})
			}
		}
		for _, count := range reply.Counts {
			if count.Kind == kind && count.Count > 0 {
				stuck = append(stuck, Stuck{
					Resource: Resource{Kind: kind, ID: count.Class, Detail: strconv.Itoa(count.Count) + " records"},
					Reason:   remainderReason(run, count.Class, kind),
				})
			}
		}
	}
	if len(stuck) == 0 {
		return
	}
	for i := range result.Stages {
		if result.Stages[i].Stage == StagePlatform {
			result.Stages[i].Stuck = append(result.Stages[i].Stuck, stuck...)
		}
	}
	slog.WarnContext(ctx, "Account teardown: L09 records remain after cleanup", "accountID", req.AccountID, "stuck", len(stuck))
}

// remainderReason is the cleanup's own reason for keeping a record, or the fixed
// unnamed-remainder reason when the cleanup never mentioned it.
func remainderReason(run *platformRun, id, kind string) string {
	if run != nil && run.cleanup != nil {
		for _, item := range run.cleanup.Items {
			if item.ResourceID == id && item.Kind == kind && item.Disposition != accountcleanup.DispositionDeleted && item.Reason != "" {
				return item.Reason
			}
		}
	}
	return operatorv1.AccountingReasonUnnamedRemainder
}
