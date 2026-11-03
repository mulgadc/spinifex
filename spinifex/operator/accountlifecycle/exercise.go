package accountlifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/nats-io/nats.go"
)

// PlatformCleaner is the EC2 platform cleanup the exercise runs.
type PlatformCleaner = accountcleanup.PlatformCleaner

// Exercise quiesces the account, cleans the EC2 platform kinds the evaluation
// recorded as quiesced, then re-reads the owner inventory and holds it against
// the cleanup. No legacy stage runs and the account record is never removed.
func (e *Engine) Exercise(ctx context.Context, req Request) (*operatorv1.ExerciseResult, error) {
	if _, err := e.checkDeletable(req.AccountID, req.AccountName); err != nil {
		return nil, err
	}
	if req.Force {
		return nil, errors.New("a non-finalizing run refuses force")
	}
	if e.Cleanup == nil || e.ec2Inventory == nil {
		return nil, errors.New("exercise needs the platform cleanup and inventory")
	}
	generation, err := e.Accounts.QuiesceAccount(req.AccountID, req.OperationID, req.Fence)
	if err != nil {
		return nil, fmt.Errorf("quiesce account: %w", err)
	}
	quiescence := e.evaluateQuiescence(ctx, req, generation, true)
	if e.OnQuiesced != nil {
		e.OnQuiesced(ctx, quiescence)
	}

	result := &operatorv1.ExerciseResult{}
	if len(quiescence.QuiescedScope) == 0 {
		result.Skipped = operatorv1.ExerciseSkippedNoQuiescedKind
	} else {
		cleaned, err := e.Cleanup.CleanupPlatform(ctx, &accountcleanup.Request{
			AccountID: req.AccountID, CorrelationRef: req.OperationID, Kinds: quiescence.QuiescedScope,
		})
		if err != nil {
			return nil, fmt.Errorf("platform cleanup: %w", err)
		}
		result.Cleanup = platformCleanup(cleaned)
		result.Accounting = e.accountForRemainder(ctx, req, quiescence.QuiescedScope, result.Cleanup)
	}
	if e.OnExercised != nil {
		e.OnExercised(ctx, *result)
	}
	return result, nil
}

// accountForRemainder re-reads the owner inventory after cleanup and holds it
// against the cleanup result, per quiesced kind.
func (e *Engine) accountForRemainder(ctx context.Context, req Request, kinds []string, cleanup *operatorv1.PlatformCleanup) []operatorv1.KindAccounting {
	reply := e.awaitPlatformInventory(ctx, req.AccountID, req.OperationID)
	return AccountForRemainder(kinds, &reply, cleanup)
}

// AccountForRemainder marks a kind accounted only when its re-read was complete
// and every record still there is named in the cleanup result with a reason.
// A record the cleanup reported deleted that is back is unnamed.
func AccountForRemainder(kinds []string, reread *ec2v1.PlatformInventoryReply, cleanup *operatorv1.PlatformCleanup) []operatorv1.KindAccounting {
	out := make([]operatorv1.KindAccounting, 0, len(kinds))
	for _, kind := range kinds {
		entry := operatorv1.KindAccounting{Kind: kind, Status: operatorv1.AccountingAccounted}
		status, complete := kindInventory(reread, kind)
		if !complete {
			entry.Status, entry.Reason = operatorv1.AccountingUnaccounted, operatorv1.AccountingReasonRereadIncomplete
			out = append(out, entry)
			continue
		}
		if status == ec2v1.PlatformStatusRecordsFound {
			unnamed := false
			for _, item := range reread.Items {
				if item.Kind != kind {
					continue
				}
				entry.Remaining++
				unnamed = unnamed || !namedItem(cleanup, item.ResourceID, kind)
			}
			for _, count := range reread.Counts {
				if count.Kind != kind {
					continue
				}
				entry.Remaining += count.Count
				unnamed = unnamed || !namedCount(cleanup, count)
			}
			if unnamed {
				entry.Status, entry.Reason = operatorv1.AccountingUnaccounted, operatorv1.AccountingReasonUnnamedRemainder
			}
		}
		out = append(out, entry)
	}
	return out
}

func namedItem(cleanup *operatorv1.PlatformCleanup, id, kind string) bool {
	if cleanup == nil {
		return false
	}
	for _, item := range cleanup.Items {
		if item.ResourceID == id && item.Kind == kind && item.Disposition != accountcleanup.DispositionDeleted && item.Reason != "" {
			return true
		}
	}
	return false
}

func namedCount(cleanup *operatorv1.PlatformCleanup, want ec2v1.PlatformRecordCount) bool {
	if cleanup == nil {
		return false
	}
	for _, count := range cleanup.Counts {
		if count.Kind == want.Kind && count.Class == want.Class && count.Disposition != accountcleanup.DispositionDeleted &&
			count.Reason != "" && count.Count >= want.Count {
			return true
		}
	}
	return false
}

func platformCleanup(res *accountcleanup.Result) *operatorv1.PlatformCleanup {
	out := &operatorv1.PlatformCleanup{Kinds: []operatorv1.CleanupKind{}}
	for _, kind := range res.Kinds {
		out.Kinds = append(out.Kinds, operatorv1.CleanupKind{Kind: kind.Kind, Status: kind.Status, IncompleteReasons: kind.IncompleteReasons})
	}
	for _, item := range res.Items {
		out.Items = append(out.Items, operatorv1.CleanupItem{
			ResourceID: item.ResourceID, Kind: item.Kind, Disposition: item.Disposition, Reason: item.Reason, MemberCount: item.MemberCount,
		})
	}
	for _, count := range res.Counts {
		out.Counts = append(out.Counts, operatorv1.CleanupCount{
			Kind: count.Kind, Class: count.Class, Disposition: count.Disposition, Reason: count.Reason, Count: count.Count,
		})
	}
	return out
}

// InstanceReferenceReader reads the key-pair and placement-group names the
// account's non-terminated instances carry. It fails closed: a listing that is
// not complete from every node is an error, never a partial answer.
type InstanceReferenceReader struct {
	nc      *nats.Conn
	nodeIDs []string
	budget  time.Duration
}

var _ accountcleanup.InstanceReferenceReader = (*InstanceReferenceReader)(nil)

// NewInstanceReferenceReader builds the reader over the configured node set.
func NewInstanceReferenceReader(nc *nats.Conn, nodeIDs []string) *InstanceReferenceReader {
	return &InstanceReferenceReader{nc: nc, nodeIDs: nodeIDs, budget: ec2PlatformInventoryBudget}
}

func (r *InstanceReferenceReader) AccountInstanceReferences(ctx context.Context, accountID string) (*accountcleanup.References, error) {
	ctx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()
	out, err := ec2instanceapi.DescribeAccountInstancesComplete(ctx, r.nc, r.nodeIDs, accountID)
	if err != nil {
		return nil, err
	}
	return instanceReferences(out), nil
}

func instanceReferences(out *ec2.DescribeInstancesOutput) *accountcleanup.References {
	refs := &accountcleanup.References{
		KeyNames: map[string]bool{}, PlacementGroupNames: map[string]bool{}, PlacementGroupIDs: map[string]bool{},
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if instance == nil {
				continue
			}
			if instance.State != nil && instance.State.Name != nil && terminalInstanceStates[*instance.State.Name] {
				continue
			}
			if instance.KeyName != nil && *instance.KeyName != "" {
				refs.KeyNames[*instance.KeyName] = true
			}
			if instance.Placement != nil {
				if instance.Placement.GroupName != nil && *instance.Placement.GroupName != "" {
					refs.PlacementGroupNames[*instance.Placement.GroupName] = true
				}
				if instance.Placement.GroupId != nil && *instance.Placement.GroupId != "" {
					refs.PlacementGroupIDs[*instance.Placement.GroupId] = true
				}
			}
		}
	}
	return refs
}
