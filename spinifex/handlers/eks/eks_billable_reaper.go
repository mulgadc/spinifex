package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// EKSBillableReaper is ADR-0006 §5's meta-independent billable cleanup, built on
// the 0003 reality→desired GC backstop. A normal DeleteCluster can leave a
// running control-plane VM behind (terminate completes async; a daemon restart
// re-launches it from saved running-state) after the cluster meta is already
// swept — a billable orphan with no owner to drive a retry. This reaper finds a
// running EKS control-plane VM whose cluster meta is DEFINITIVELY GONE and
// terminates it. It is node-local: each node reaps the orphans it runs.
type EKSBillableReaper struct {
	svc *EKSServiceImpl
	// running returns this node's running VMs. The reaper filters to the EKS
	// control-plane VMs and checks each against its cluster meta.
	running func() ([]*vm.VM, error)
}

var _ vm.Reaper = (*EKSBillableReaper)(nil)

// NewBillableReaper builds the EKS billable-orphan reaper. running supplies this
// node's running VMs (from the daemon's StateStore).
func (s *EKSServiceImpl) NewBillableReaper(running func() ([]*vm.VM, error)) *EKSBillableReaper {
	return &EKSBillableReaper{svc: s, running: running}
}

func (r *EKSBillableReaper) Class() string         { return "eks-billable" }
func (r *EKSBillableReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep terminates every running EKS control-plane VM whose cluster meta is
// absent. A VM whose cluster still exists (CREATING or DELETING) is left to the
// cluster's own teardown/retry; an ENI that is gone or untagged is skipped, so
// the reaper never reaps on uncertainty.
func (r *EKSBillableReaper) Sweep(ctx context.Context) (int, error) {
	vms, err := r.running()
	if err != nil {
		return 0, fmt.Errorf("list running VMs: %w", err)
	}

	js, err := r.svc.deps.NATSConn.JetStream()
	if err != nil {
		return 0, fmt.Errorf("jetstream: %w", err)
	}

	reaped := 0
	for _, v := range vms {
		select {
		case <-ctx.Done():
			return reaped, ctx.Err()
		default:
		}
		if v == nil || v.ManagedBy != tags.ManagedByEKS || v.ENIId == "" {
			continue
		}

		clusterName, clusterAccount, ok := r.clusterRefFromENI(ctx, v)
		if !ok {
			continue // ENI gone or untagged: cannot confirm orphan-hood, never reap
		}

		acctKV, err := GetOrCreateAccountBucket(js, clusterAccount, max(r.svc.deps.ClusterSize, 1))
		if err != nil {
			slog.WarnContext(ctx, "eks-billable: account bucket lookup failed", "account", clusterAccount, "err", err)
			continue
		}
		if _, err := GetClusterMeta(acctKV, clusterName); !errors.Is(err, ErrClusterNotFound) {
			// Meta present (live/CREATING/DELETING) or unreadable: not a definitive
			// orphan — leave it to the cluster's own teardown/retry.
			continue
		}

		// ALARM + reap: the cluster is gone but its control-plane VM still runs.
		slog.WarnContext(ctx, "DATA-SAFETY ALARM: orphaned EKS control-plane VM reaped (cluster meta gone)",
			"instanceId", v.ID, "cluster", clusterName, "account", clusterAccount, "node", v.LastNode)
		if err := TerminateK3sServerVM(ctx, r.svc.deps.VPCK3s, r.svc.deps.Instance, v.AccountID, v.ID, v.ENIId); err != nil {
			slog.WarnContext(ctx, "eks-billable: failed to terminate orphan CP VM", "instanceId", v.ID, "err", err)
			continue
		}
		reaped++
		r.reclaimOrphanInfra(ctx, v.AccountID, clusterName)
	}
	return reaped, nil
}

// reclaimOrphanInfra tears down the billable infra a reaped orphan's swept meta
// no longer anchors: the NLB front-end NAT and the managed CP VPC's NAT-GW EIP.
// Both are keyed by cluster name and idempotent, so a clean prior delete is a
// no-op. Best-effort — the costliest resource (the VM) is already gone, and
// DeleteClusterCPVPC releases the NAT-GW EIP before the VPC delete, so the
// billable address is reclaimed even if the now-empty VPC delete trails. account
// is the infra account (system account for the managed CP VPC topology).
func (r *EKSBillableReaper) reclaimOrphanInfra(ctx context.Context, account, clusterName string) {
	if err := DeleteClusterNLB(ctx, r.svc.deps.NLB, account, clusterName); err != nil {
		slog.WarnContext(ctx, "eks-billable: reclaim orphan NLB failed", "cluster", clusterName, "err", err)
	}
	if err := DeleteClusterCPVPC(ctx, r.svc.cpVPCDeps(), account, clusterName); err != nil {
		slog.WarnContext(ctx, "eks-billable: reclaim orphan CP VPC (NAT-GW EIP) failed", "cluster", clusterName, "err", err)
	}
}

// clusterRefFromENI reads the cluster name + customer account from the VM's
// control-plane ENI tags. ok is false when the ENI is gone or missing either tag.
func (r *EKSBillableReaper) clusterRefFromENI(ctx context.Context, v *vm.VM) (clusterName, clusterAccount string, ok bool) {
	out, err := r.svc.deps.VPCK3s.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: aws.StringSlice([]string{v.ENIId}),
	}, v.AccountID)
	if err != nil || out == nil || len(out.NetworkInterfaces) == 0 {
		return "", "", false
	}
	for _, tag := range out.NetworkInterfaces[0].TagSet {
		switch aws.StringValue(tag.Key) {
		case clusterEKSClusterTagKey:
			clusterName = aws.StringValue(tag.Value)
		case clusterEKSAccountTagKey:
			clusterAccount = aws.StringValue(tag.Value)
		}
	}
	if clusterName == "" || clusterAccount == "" {
		return "", "", false
	}
	return clusterName, clusterAccount, true
}
