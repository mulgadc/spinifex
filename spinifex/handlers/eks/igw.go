package handlers_eks

import (
	"context"
	"errors"

	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
)

// igwProvisioner is the Internet Gateway surface the cluster IGW helpers need,
// under its EKS-local name. The daemon adapts the concrete IGW service onto it.
type igwProvisioner = handlers_systemvpc.IGWProvisioner

// EnsureClusterIGW guarantees vpcID has an attached Internet Gateway so an
// internet-facing cluster endpoint is reachable. An already-attached IGW is
// reused as-is; only when none exists is a cluster-owned one created.
func EnsureClusterIGW(ctx context.Context, igwp igwProvisioner, accountID, vpcID, clusterName string) error {
	if clusterName == "" {
		return errors.New("eks: EnsureClusterIGW empty cluster name")
	}
	return handlers_systemvpc.EnsureIGW(ctx, igwp, cpVPCOwner(clusterName), accountID, vpcID)
}

// DeleteClusterIGW detaches and deletes the cluster-owned IGW attached to vpcID.
// Best-effort and ownership-scoped: a customer-provisioned IGW that
// EnsureClusterIGW reused is never deleted.
func DeleteClusterIGW(ctx context.Context, igwp igwProvisioner, accountID, vpcID, clusterName string) error {
	if clusterName == "" {
		return errors.New("eks: DeleteClusterIGW empty cluster name")
	}
	return handlers_systemvpc.DeleteIGW(ctx, igwp, cpVPCOwner(clusterName), accountID, vpcID)
}
