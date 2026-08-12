package daemon

import (
	"context"
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

// eniOrphanReaper deletes ENI records left behind by a launch that created the
// interface and then died before attaching it. The record outlives the launch,
// and because the security group dependency check counts an ENI that lists the
// group, the SG can never be deleted afterwards.
//
// Cluster-wide rather than node-local: the record has no instance, so no node
// owns it, and nothing local would ever look at it again.
type eniOrphanReaper struct {
	vpc    *handlers_ec2_vpc.VPCServiceImpl
	minAge time.Duration
}

var _ vm.Reaper = (*eniOrphanReaper)(nil)

// newENIOrphanReaper builds the sweep over the daemon's VPC service, or nil
// when there is no control plane on this node to sweep with.
func (d *Daemon) newENIOrphanReaper() *eniOrphanReaper {
	if d.vpcService == nil {
		return nil
	}
	return &eniOrphanReaper{vpc: d.vpcService, minAge: eniOrphanMinAge}
}

func (r *eniOrphanReaper) Class() string         { return "eni-orphan" }
func (r *eniOrphanReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweep deletes every abandoned instance ENI. A per-record failure is logged
// and skipped so one bad record cannot stall the rest of the sweep.
func (r *eniOrphanReaper) Sweep(ctx context.Context) (int, error) {
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
