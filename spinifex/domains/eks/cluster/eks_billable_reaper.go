package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// EKSBillableReaper raises the data-safety alarm for a running EKS control-plane
// VM whose cluster record cannot be found. It never terminates anything: a
// missing record cannot tell a completed DeleteCluster from a lost bucket or an
// out-of-band delete, so removal is an operator decision. It is node-local.
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

// Sweep alarms on every running EKS control-plane VM whose cluster record, or
// whole account bucket, is missing, and removes nothing. A VM whose cluster
// exists is left to the cluster's own teardown; an ENI that is gone or untagged
// is skipped, and a read that fails is logged and skipped. It returns 0 because
// it never removes a VM.
func (r *EKSBillableReaper) Sweep(ctx context.Context) (int, error) {
	vms, err := r.running()
	if err != nil {
		return 0, fmt.Errorf("list running VMs: %w", err)
	}

	js, err := jetstream.New(r.svc.deps.NATSConn)
	if err != nil {
		return 0, fmt.Errorf("jetstream: %w", err)
	}

	for _, v := range vms {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		if v == nil || v.ManagedBy != tags.ManagedByEKS || v.ENIId == "" {
			continue
		}

		clusterName, clusterAccount, ok := r.clusterRefFromENI(ctx, v)
		if !ok {
			continue // ENI gone or untagged: cannot tie the VM to a cluster
		}

		reason := "cluster record missing"
		acctKV, err := OpenAccountBucket(ctx, js, clusterAccount)
		switch {
		case errors.Is(err, jetstream.ErrBucketNotFound):
			reason = "account bucket missing"
		case err != nil:
			slog.WarnContext(ctx, "eks-billable: account bucket lookup failed", "account", clusterAccount, "err", err)
			continue
		default:
			_, err = GetClusterMeta(ctx, acctKV, clusterName)
			if err == nil {
				continue // cluster exists (live, CREATING or DELETING)
			}
			if !errors.Is(err, ErrClusterNotFound) {
				slog.WarnContext(ctx, "eks-billable: cluster record read failed", "cluster", clusterName, "account", clusterAccount, "err", err)
				continue
			}
		}

		slog.WarnContext(ctx, "DATA-SAFETY ALARM: EKS control-plane VM has no cluster record; retained for an operator",
			"reason", reason, "instanceId", v.ID, "cluster", clusterName, "account", clusterAccount, "node", v.LastNode)
	}
	return 0, nil
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
