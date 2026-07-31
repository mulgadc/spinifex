package handlers_eks

import (
	"context"

	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// The managed control-plane VPC is the spinifex analogue of AWS EKS's hidden
// managed-account VPC: CreateCluster builds it, the system account owns it, and
// the customer never sees it. The NLB lives in its public subnet, the k3s
// control-plane VMs in its NAT-routed private subnets.
//
// The topology is the shared systemvpc builder; EKS supplies only the identity
// — its own tag keys, so no other component's sweep reaches these resources —
// and the address space.
const (
	// cpVPCRolePrefix namespaces the CP VPC's role tag values ("cp-vpc",
	// "cp-public", …). Baked into live deployments' tags: changing it orphans
	// every existing CP VPC from its teardown.
	cpVPCRolePrefix = "cp"

	// cpVPCSupernet anchors the managed CP VPC address space at 10.252.0.0/14.
	// Each cluster gets a deterministic /22 carved from it: a public /24 + up to
	// three private /24s (HA 3-node etcd spread).
	cpVPCSupernet = "10.252.0.0/14"
)

// cpVPCPrivateSubnetCount is how many private CP subnets the managed VPC carves.
// One today: the placement rule is identical for single- and multi-node
// clusters, so all control-plane VMs share the private subnet.
const cpVPCPrivateSubnetCount = 1

// cpVPCRoles are the role-tag values the managed CP VPC's resources carry.
var cpVPCRoles = handlers_systemvpc.Spec{RolePrefix: cpVPCRolePrefix}.Roles()

// CPVPCDeps and ManagedCPVPCRefs are the systemvpc types under the names EKS
// callers (and ClusterMeta's projection) already use.
type (
	CPVPCDeps        = handlers_systemvpc.Deps
	ManagedCPVPCRefs = handlers_systemvpc.Refs
)

// The provisioner surfaces the CP VPC composes from, under their EKS-local
// names so the service deps and their fakes read against one vocabulary.
type (
	vpcProvisioner        = handlers_systemvpc.VPCProvisioner
	routeTableProvisioner = handlers_systemvpc.RouteTableProvisioner
	natGatewayProvisioner = handlers_systemvpc.NATGatewayProvisioner
)

// cpVPCOwner is the tag identity every managed CP VPC resource carries. Keyed on
// the EKS cluster tags, so the EKS teardown and billable reapers see exactly
// these resources and no other component's.
func cpVPCOwner(clusterName string) handlers_systemvpc.Owner {
	return handlers_systemvpc.Owner{
		Name:        clusterName,
		ManagedBy:   tags.ManagedByEKS,
		OwnerTagKey: clusterEKSClusterTagKey,
		RoleTagKey:  clusterEKSRoleTagKey,
	}
}

// cpVPCSpec is the full build spec for clusterName's managed control-plane VPC.
func cpVPCSpec(clusterName, region string, privateCount int) handlers_systemvpc.Spec {
	return handlers_systemvpc.Spec{
		Owner:          cpVPCOwner(clusterName),
		Region:         region,
		RolePrefix:     cpVPCRolePrefix,
		Supernet:       cpVPCSupernet,
		PrivateSubnets: privateCount,
	}
}

// managedCPVPCFromRefs projects the resolved refs onto the persisted ClusterMeta
// shape used for teardown.
func managedCPVPCFromRefs(r *ManagedCPVPCRefs) *ManagedCPVPC {
	if r == nil {
		return nil
	}
	return &ManagedCPVPC{
		VpcId:               r.VpcID,
		IGWId:               r.IGWID,
		PublicSubnetId:      r.PublicSubnetID,
		PublicRouteTableId:  r.PublicRouteTableID,
		PrivateSubnetIds:    r.PrivateSubnetIDs,
		PrivateRouteTableId: r.PrivateRouteTableID,
		NatGatewayId:        r.NatGatewayID,
		NatEIPAllocationID:  r.NatEIPAllocationID,
		NatEIPPublicIP:      r.NatEIPPublicIP,
	}
}

// cpVPCDeps adapts the service's EC2-family deps onto CPVPCDeps for the managed
// control-plane VPC build + teardown.
func (s *EKSServiceImpl) cpVPCDeps() CPVPCDeps {
	return CPVPCDeps{
		VPC:      s.deps.VPCMgr,
		SG:       s.deps.VPCSG,
		IGW:      s.deps.IGW,
		RT:       s.deps.RouteTable,
		NGW:      s.deps.NATGW,
		EIP:      s.deps.EIP,
		NATSConn: s.deps.NATSConn,
	}
}

// EnsureClusterCPVPC builds (idempotently) the managed control-plane VPC under
// accountID (the system account) for clusterName.
func EnsureClusterCPVPC(ctx context.Context, deps CPVPCDeps, accountID, clusterName, region string, privateCount int) (*ManagedCPVPCRefs, error) {
	return handlers_systemvpc.Ensure(ctx, deps, cpVPCSpec(clusterName, region, privateCount), accountID)
}

// DeleteClusterCPVPC tears down the managed control-plane VPC for clusterName.
//
// knownRefs is the caller's last-persisted cp-vpc record, used only as an OVN-GC
// fallback when the tag-indexed EC2 VPC is already gone. Pass nil when no such
// record exists, e.g. the billable reaper acting after the cluster meta.
func DeleteClusterCPVPC(ctx context.Context, deps CPVPCDeps, accountID, clusterName string, knownRefs *ManagedCPVPC) error {
	gcFallback := ""
	if knownRefs != nil {
		gcFallback = knownRefs.VpcId
	}
	// Region and private-subnet count do not affect teardown: every lookup is a
	// describe-by-tag, and the CIDRs are never re-derived.
	return handlers_systemvpc.Delete(ctx, deps, cpVPCSpec(clusterName, "", cpVPCPrivateSubnetCount), accountID, gcFallback)
}
