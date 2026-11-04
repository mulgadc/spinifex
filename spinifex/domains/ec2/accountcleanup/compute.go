package accountcleanup

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// Compute kinds and the extra dispositions and reasons the compute run reports.
const (
	KindInstance    = "instance"
	KindSpotRequest = "spot-request"

	// DispositionCancelled: a spot request this run moved to cancelled.
	DispositionCancelled = "cancelled"
	// DispositionTerminal: a spot request that was already closed, cancelled or failed.
	DispositionTerminal = "terminal"
	// DispositionPending: a terminate was acknowledged but the teardown read cannot
	// confirm completion, because terminated records expire.
	DispositionPending = "pending"

	// ReasonTerminating: a terminate is in progress or its teardown is unfinished.
	ReasonTerminating = "terminating"
	// ReasonTerminationUnproven: the terminate was not acknowledged, so the
	// instance may still be running on a node that did not answer.
	ReasonTerminationUnproven = "termination-unproven"
	// ReasonManagedServiceOwner: a managed service still owns the instance.
	ReasonManagedServiceOwner = "managed-service-owner"
	// ReasonSpotCancelFailed: a spot request that can still launch could not be cancelled.
	ReasonSpotCancelFailed = "spot-cancel-failed"
	// ReasonSpotStateUnrecognised: a spot request in a state outside the EC2 model.
	ReasonSpotStateUnrecognised = "spot-state-unrecognised"
	// ReasonTeardownUnconfirmed: no unfinished teardown was observed, which is
	// not proof the teardown finished.
	ReasonTeardownUnconfirmed = "teardown-unconfirmed"

	// Incomplete reasons: the run stopped or could not prove a part of its result.
	ReasonSpotInventoryIncomplete     = "spot-inventory-incomplete"
	ReasonInstanceInventoryIncomplete = "instance-inventory-incomplete"
	ReasonManagedOwnershipUnknown     = "managed-ownership-unknown"
	ReasonTeardownUnread              = "teardown-unread"
)

// Compute run statuses.
const (
	StatusClean      = "clean"
	StatusIncomplete = "incomplete"
)

// ComputeRequest names the account whose compute is cleaned. Force is accepted
// for callers that carry one and bypasses nothing: an unproven terminate, an
// incomplete inventory or a managed owner is never overridden.
type ComputeRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
	Force          bool   `json:"force,omitempty"`
}

// ComputeResult is what one compute run found and did. Clean means nothing was
// blocked and every read completed; it is a snapshot, not evidence of absence.
type ComputeResult struct {
	AccountID         string   `json:"account_id"`
	CorrelationRef    string   `json:"correlation_ref,omitempty"`
	Status            string   `json:"status"`
	IncompleteReasons []string `json:"incomplete_reasons,omitempty"`
	Items             []Item   `json:"items,omitempty"`
}

// ComputeCleaner terminates an account's instances and cancels its spot
// requests. It is destructive; the caller must hold an accepted account deletion.
type ComputeCleaner interface {
	CleanupCompute(ctx context.Context, req *ComputeRequest) (*ComputeResult, error)
}

// SpotRequestOwner is the spot-request owner's account scan and cancel.
type SpotRequestOwner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2spotinstance.AccountScan, error)
	CancelSpotInstanceRequests(ctx context.Context, input *ec2.CancelSpotInstanceRequestsInput, accountID string) (*ec2.CancelSpotInstanceRequestsOutput, error)
}

// InstanceInventory lists every non-terminated instance the account owns, or
// fails. It is DescribeAccountInstancesStrict; a partial list is never returned.
type InstanceInventory interface {
	AccountInstances(ctx context.Context, accountID string) (*ec2.DescribeInstancesOutput, error)
}

// InstanceTerminator sends the existing EC2 terminate for one instance and
// returns once the owning node acknowledged it.
type InstanceTerminator interface {
	TerminateInstance(ctx context.Context, accountID, instanceID string) error
}

// TeardownReader reports instances whose terminate is not fully torn down.
type TeardownReader interface {
	AccountTeardownOutstanding(accountID string) ([]ec2instance.OutstandingInstance, error)
}

// ManagedInstanceReader reads the complete set of instance IDs a managed
// service, such as an EKS nodegroup, still owns in the account, or an error.
// The owner of that record answers it; EC2 never reads those records.
type ManagedInstanceReader interface {
	AccountManagedInstances(ctx context.Context, accountID string) (map[string]bool, error)
}

var (
	_ SpotRequestOwner = (*ec2spotinstance.SpotInstanceServiceImpl)(nil)
	_ TeardownReader   = (*ec2instance.InstanceServiceImpl)(nil)
	_ ComputeCleaner   = (*ComputeCleanup)(nil)
)

// ComputeCleanup composes the compute owners.
type ComputeCleanup struct {
	spot      SpotRequestOwner
	instances InstanceInventory
	terminate InstanceTerminator
	teardown  TeardownReader
	managed   ManagedInstanceReader
}

// NewCompute builds a compute cleanup. A nil dependency makes every run fail
// before it acts: a missing managed-instance reader is a wiring error, not "none owned".
func NewCompute(spot SpotRequestOwner, instances InstanceInventory, terminate InstanceTerminator, teardown TeardownReader, managed ManagedInstanceReader) *ComputeCleanup {
	return &ComputeCleanup{spot: spot, instances: instances, terminate: terminate, teardown: teardown, managed: managed}
}

// CleanupCompute cancels the account's open and active spot requests, then
// terminates every instance that is not already terminating and no managed
// service owns. It never waits for teardown: outstanding work is reported and a
// rerun converges it. A spot, inventory or ownership read that does not
// complete terminates nothing.
func (c *ComputeCleanup) CleanupCompute(ctx context.Context, req *ComputeRequest) (*ComputeResult, error) {
	if req == nil || req.AccountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must specify an account ID.")
	}
	if !awsidentifiers.IsAccountID(req.AccountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The account ID must be 12 digits.")
	}
	if c == nil || c.spot == nil || c.instances == nil || c.terminate == nil || c.teardown == nil || c.managed == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "Compute cleanup is not fully wired.")
	}
	res := &ComputeResult{AccountID: req.AccountID, CorrelationRef: req.CorrelationRef}
	defer res.finish()

	if !c.cancelSpot(ctx, req.AccountID, res) {
		return res, nil
	}

	inventory, err := c.instances.AccountInstances(ctx, req.AccountID)
	if err != nil {
		slog.WarnContext(ctx, "CleanupCompute: instance inventory incomplete, nothing terminated", "account_id", req.AccountID, "err", err)
		res.incomplete(ReasonInstanceInventoryIncomplete)
		return res, nil
	}
	managed, err := c.managed.AccountManagedInstances(ctx, req.AccountID)
	if err != nil {
		slog.WarnContext(ctx, "CleanupCompute: managed ownership unknown, nothing terminated", "account_id", req.AccountID, "err", err)
		res.incomplete(ReasonManagedOwnershipUnknown)
		return res, nil
	}

	if !inventoryUsable(inventory) {
		slog.WarnContext(ctx, "CleanupCompute: inventory holds an entry with no identity, nothing terminated", "account_id", req.AccountID)
		res.incomplete(ReasonInstanceInventoryIncomplete)
		return res, nil
	}

	asked := c.terminateAll(ctx, req.AccountID, inventory, managed, res)
	c.reportTeardown(req.AccountID, asked, res)
	return res, nil
}

// cancelSpot cancels open and active requests and rescans. It reports false
// when the spot state is not proven, which stops the run before any terminate.
func (c *ComputeCleanup) cancelSpot(ctx context.Context, accountID string, res *ComputeResult) bool {
	scan, err := c.spot.ScanAccount(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "CleanupCompute: spot scan failed, nothing terminated", "account_id", accountID, "err", err)
		res.incomplete(ReasonSpotInventoryIncomplete)
		return false
	}
	var open []*string
	cancelled := map[string]bool{}
	for _, r := range scan.Active {
		if !terminalSpotState(r.State) {
			open = append(open, aws.String(r.RequestID))
			cancelled[r.RequestID] = true
		}
	}
	if len(open) > 0 {
		if _, err := c.spot.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: open}, accountID); err != nil {
			slog.WarnContext(ctx, "CleanupCompute: spot cancel failed, nothing terminated", "account_id", accountID, "err", err)
			for _, id := range open {
				res.Items = append(res.Items, Item{ResourceID: aws.StringValue(id), Kind: KindSpotRequest, Disposition: DispositionBlocked, Reason: ReasonSpotCancelFailed})
			}
			res.incomplete(ReasonSpotInventoryIncomplete)
			return false
		}
		scan, err = c.spot.ScanAccount(ctx, accountID)
		if err != nil {
			slog.WarnContext(ctx, "CleanupCompute: spot rescan failed, nothing terminated", "account_id", accountID, "err", err)
			res.incomplete(ReasonSpotInventoryIncomplete)
			return false
		}
	}
	proven := true
	for _, r := range scan.Active {
		if !terminalSpotState(r.State) {
			proven = false
			res.Items = append(res.Items, Item{ResourceID: r.RequestID, Kind: KindSpotRequest, State: r.State, Disposition: DispositionBlocked, Reason: ReasonSpotCancelFailed})
		}
	}
	for _, r := range scan.Terminal {
		if !terminalSpotState(r.State) {
			proven = false
			res.Items = append(res.Items, Item{ResourceID: r.RequestID, Kind: KindSpotRequest, State: r.State, Disposition: DispositionBlocked, Reason: ReasonSpotStateUnrecognised})
			continue
		}
		disposition := DispositionTerminal
		if cancelled[r.RequestID] {
			disposition = DispositionCancelled
		}
		res.Items = append(res.Items, Item{ResourceID: r.RequestID, Kind: KindSpotRequest, State: r.State, Disposition: disposition})
	}
	if !proven {
		res.incomplete(ReasonSpotInventoryIncomplete)
	}
	return proven
}

// terminalSpotState: closed, cancelled and failed requests launch nothing more.
// Every other state, disabled and unknown ones included, must be cancelled.
func terminalSpotState(state string) bool {
	return state == ec2.SpotInstanceStateClosed || state == ec2.SpotInstanceStateCancelled || state == ec2.SpotInstanceStateFailed
}

// inventoryUsable reports whether every entry names an instance. An entry that
// does not could be a live instance, so the run terminates nothing.
func inventoryUsable(out *ec2.DescribeInstancesOutput) bool {
	if out == nil {
		return false
	}
	for _, reservation := range out.Reservations {
		if reservation == nil {
			return false
		}
		for _, in := range reservation.Instances {
			if in == nil || aws.StringValue(in.InstanceId) == "" {
				return false
			}
		}
	}
	return true
}

// terminateAll terminates every listed instance that is not terminating and
// not managed, and returns the IDs whose terminate is in hand.
func (c *ComputeCleanup) terminateAll(ctx context.Context, accountID string, inventory *ec2.DescribeInstancesOutput, managed map[string]bool, res *ComputeResult) map[string]bool {
	asked := map[string]bool{}
	for _, reservation := range inventory.Reservations {
		for _, in := range reservation.Instances {
			id := aws.StringValue(in.InstanceId)
			state := ""
			if in.State != nil {
				state = aws.StringValue(in.State.Name)
			}
			switch {
			case managed[id]:
				res.Items = append(res.Items, Item{ResourceID: id, Kind: KindInstance, State: state, Disposition: DispositionBlocked, Reason: ReasonManagedServiceOwner})
			case state == ec2.InstanceStateNameTerminated, state == ec2.InstanceStateNameShuttingDown:
				// Already terminating: the teardown read below reports it.
				asked[id] = true
			default:
				if err := c.terminate.TerminateInstance(ctx, accountID, id); err != nil {
					slog.WarnContext(ctx, "CleanupCompute: terminate not acknowledged", "account_id", accountID, "instance_id", id, "err", err)
					res.Items = append(res.Items, Item{ResourceID: id, Kind: KindInstance, State: state, Disposition: DispositionBlocked, Reason: ReasonTerminationUnproven})
					continue
				}
				asked[id] = true
			}
		}
	}
	return asked
}

// reportTeardown reads the unfinished teardowns once, without waiting, and
// reports each instance still in progress as blocked.
func (c *ComputeCleanup) reportTeardown(accountID string, asked map[string]bool, res *ComputeResult) {
	outstanding, err := c.teardown.AccountTeardownOutstanding(accountID)
	if err != nil {
		res.incomplete(ReasonTeardownUnread)
		for id := range asked {
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindInstance, Disposition: DispositionBlocked, Reason: ReasonTerminationUnproven})
		}
		return
	}
	open := map[string]bool{}
	for _, o := range outstanding {
		open[o.InstanceID] = true
		if !res.hasInstance(o.InstanceID) {
			res.Items = append(res.Items, Item{ResourceID: o.InstanceID, Kind: KindInstance, State: o.State, Disposition: DispositionBlocked, Reason: ReasonTerminating})
		}
	}
	for id := range asked {
		if !open[id] {
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindInstance, Disposition: DispositionPending, Reason: ReasonTeardownUnconfirmed})
		}
	}
}

func (r *ComputeResult) hasInstance(id string) bool {
	return slices.ContainsFunc(r.Items, func(it Item) bool { return it.Kind == KindInstance && it.ResourceID == id })
}

func (r *ComputeResult) incomplete(reason string) {
	if !slices.Contains(r.IncompleteReasons, reason) {
		r.IncompleteReasons = append(r.IncompleteReasons, reason)
	}
}

func (r *ComputeResult) finish() {
	r.Status = StatusClean
	if len(r.IncompleteReasons) > 0 {
		r.Status = StatusIncomplete
	}
	for _, it := range r.Items {
		if it.Disposition == DispositionBlocked || it.Disposition == DispositionPending {
			r.Status = StatusIncomplete
		}
	}
	slices.SortFunc(r.Items, func(a, b Item) int {
		return strings.Compare(a.Kind+a.ResourceID, b.Kind+b.ResourceID)
	})
}
