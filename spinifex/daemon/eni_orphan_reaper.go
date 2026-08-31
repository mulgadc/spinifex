package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// eniOrphanMinAge is how long an unattached instance ENI must sit before the
// sweep will delete it. The launch RPC gives a node five minutes to answer, so
// this leaves a wide margin past the point where a launch could still attach.
const eniOrphanMinAge = 15 * time.Minute

// instanceIndex answers whether the control plane still knows an instance. Both
// listings together cover an instance's whole life: the shared record space
// holds it while it runs and while it is stopped, and the terminated bucket
// holds it for the hour after it goes away.
type instanceIndex interface {
	ListInstanceRecords() ([]*vm.InstanceRecord, error)
	ListTerminatedInstanceRecords() ([]*vm.InstanceRecord, error)
}

// eipDisassociator releases the association an Elastic IP holds on an ENI,
// leaving the allocation itself in place.
type eipDisassociator interface {
	DisassociateByENI(ctx context.Context, accountID, eniID string) (bool, error)
}

// eniOrphanReaper deletes ENI records no instance can still be using. Two shapes
// qualify, and neither has anything else that would ever remove it:
//
// A launch that created the interface and then died before attaching it leaves a
// record with no instance at all. Because the security group dependency check
// counts an ENI that lists the group, the SG can never be deleted afterwards.
//
// A teardown that never finished leaves the opposite: a record still naming an
// instance that exists nowhere. The node-local teardown reaper would have
// re-driven it, but only from the terminated bucket, which expires after an
// hour — past that the ENI, its logical switch port and its EIP association are
// permanent, and the network reconciler keeps driving toward a port that can
// never bind.
//
// Cluster-wide rather than node-local: neither shape has a live instance, so no
// node owns it, and nothing local would ever look at it again.
type eniOrphanReaper struct {
	vpc       *handlers_ec2_vpc.VPCServiceImpl
	instances instanceIndex
	eip       eipDisassociator
	minAge    time.Duration
}

var _ vm.Reaper = (*eniOrphanReaper)(nil)

// newENIOrphanReaper builds the sweep over the daemon's VPC service, or nil
// when there is no control plane on this node to sweep with. A nil jsManager or
// EIP service degrades the sweep rather than disabling it: see Sweep.
func (d *Daemon) newENIOrphanReaper() *eniOrphanReaper {
	if d.vpcService == nil {
		return nil
	}
	r := &eniOrphanReaper{vpc: d.vpcService, minAge: eniOrphanMinAge}
	if d.jsManager != nil {
		r.instances = d.jsManager
	}
	if eip, ok := d.eipService.(eipDisassociator); ok {
		r.eip = eip
	}
	return r
}

func (r *eniOrphanReaper) Class() string         { return "eni-orphan" }
func (r *eniOrphanReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweep deletes every abandoned launch ENI and every ENI whose instance is gone.
// A per-record failure is logged and skipped so one bad record cannot stall the
// rest of the sweep.
func (r *eniOrphanReaper) Sweep(ctx context.Context) (int, error) {
	reaped, err := r.sweepAbandonedLaunches(ctx)
	if err != nil {
		return reaped, err
	}
	stale, err := r.sweepStaleAttachments(ctx)
	return reaped + stale, err
}

// sweepAbandonedLaunches deletes ENIs that never reached an attachment. A plain
// delete suffices: with no attachment there is no in-use guard to clear.
func (r *eniOrphanReaper) sweepAbandonedLaunches(ctx context.Context) (int, error) {
	orphans, err := r.vpc.ListAbandonedInstanceENIs(ctx, r.minAge)
	if err != nil {
		return 0, err
	}

	reaped := 0
	for _, orphan := range orphans {
		if ctx.Err() != nil {
			return reaped, ctx.Err()
		}
		eniID := orphan.Record.NetworkInterfaceId
		slog.InfoContext(ctx, "eni-orphan: deleting ENI left by an abandoned launch",
			"eniId", eniID,
			"accountId", orphan.AccountID,
			"description", orphan.Record.Description,
			"securityGroups", orphan.Record.SecurityGroupIds,
			"createdAt", orphan.Record.CreatedAt)

		_, err := r.vpc.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(eniID),
		}, orphan.AccountID)
		// Another sweep may have deleted it between the list and here, which
		// is the outcome this asked for either way.
		if err != nil && err.Error() != awserrors.ErrorInvalidNetworkInterfaceIDNotFound {
			slog.WarnContext(ctx, "eni-orphan: delete failed",
				"eniId", eniID, "accountId", orphan.AccountID, "err", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

// sweepStaleAttachments deletes ENIs still naming an instance the control plane
// no longer holds anywhere. The delete uses the force path, which exists for
// exactly this owner-driven teardown; the in-use guard is left alone, since it
// is right to reject a delete aimed at another live instance's interface.
func (r *eniOrphanReaper) sweepStaleAttachments(ctx context.Context) (int, error) {
	if r.instances == nil {
		return 0, nil
	}
	known, err := r.knownInstanceIDs()
	if err != nil {
		// Fail closed. A short listing reads as "the instance is gone" for every
		// instance missing from it, which would reap live guests' interfaces.
		slog.WarnContext(ctx, "eni-orphan: instance listing failed; skipping stale-attachment sweep", "err", err)
		return 0, nil
	}

	attached, err := r.vpc.ListAttachedInstanceENIs(ctx, r.minAge)
	if err != nil {
		return 0, err
	}

	reaped := 0
	for _, candidate := range attached {
		if ctx.Err() != nil {
			return reaped, ctx.Err()
		}
		if known[candidate.Record.InstanceId] {
			continue
		}
		if r.reapStale(ctx, candidate) {
			reaped++
		}
	}
	return reaped, nil
}

// reapStale releases one zombie ENI's EIP association and then the ENI itself,
// reporting whether the ENI is gone. The EIP goes first: the disassociation
// reads the record's captured MAC to withdraw the NAT rule, and leaving it for
// afterwards would leave the address associated to an interface that no longer
// exists if the delete failed.
func (r *eniOrphanReaper) reapStale(ctx context.Context, candidate handlers_ec2_vpc.AccountENI) bool {
	eniID := candidate.Record.NetworkInterfaceId
	slog.InfoContext(ctx, "eni-orphan: deleting ENI whose instance no longer exists",
		"eniId", eniID,
		"accountId", candidate.AccountID,
		"instanceId", candidate.Record.InstanceId,
		"privateIp", candidate.Record.PrivateIpAddress,
		"createdAt", candidate.Record.CreatedAt)

	r.releaseEIP(ctx, candidate.AccountID, eniID)

	if err := r.vpc.ForceDeleteInstanceENI(ctx, candidate.AccountID, eniID); err != nil {
		slog.WarnContext(ctx, "eni-orphan: stale ENI delete failed",
			"eniId", eniID, "accountId", candidate.AccountID, "err", err)
		return false
	}
	return true
}

// releaseEIP drops any EIP association held on eniID. Best-effort: a failure
// here must not stop the ENI delete, and the next sweep retries it.
func (r *eniOrphanReaper) releaseEIP(ctx context.Context, accountID, eniID string) {
	if r.eip == nil {
		return
	}
	found, err := r.eip.DisassociateByENI(ctx, accountID, eniID)
	if err != nil {
		slog.WarnContext(ctx, "eni-orphan: EIP disassociate failed",
			"eniId", eniID, "accountId", accountID, "err", err)
		return
	}
	if found {
		slog.InfoContext(ctx, "eni-orphan: released EIP association from a stale ENI",
			"eniId", eniID, "accountId", accountID)
	}
}

// knownInstanceIDs is every instance the control plane still holds a record for,
// running, stopped or recently terminated.
func (r *eniOrphanReaper) knownInstanceIDs() (map[string]bool, error) {
	live, err := r.instances.ListInstanceRecords()
	if err != nil {
		return nil, fmt.Errorf("list instance records: %w", err)
	}
	terminated, err := r.instances.ListTerminatedInstanceRecords()
	if err != nil {
		return nil, fmt.Errorf("list terminated instance records: %w", err)
	}

	known := make(map[string]bool, len(live)+len(terminated))
	for _, record := range live {
		known[record.Metadata.Name] = true
	}
	for _, record := range terminated {
		known[record.Metadata.Name] = true
	}
	return known, nil
}
