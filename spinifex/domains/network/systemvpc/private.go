package systemvpc

import (
	"context"
	"log/slog"
)

// PrivateRefs identifies a private-only system VPC and its one subnet.
type PrivateRefs struct {
	VpcID      string
	VpcCIDR    string
	SubnetID   string
	SubnetCIDR string
}

// EnsurePrivate idempotently builds a VPC with one private subnet and nothing
// else. It takes only the VPC and subnet surface, so it cannot create an IGW,
// route table, NAT gateway or EIP. Use a Spec with either this or Ensure.
func EnsurePrivate(ctx context.Context, vpcp VPCProvisioner, spec Spec, accountID string) (*PrivateRefs, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	vpcCIDR, _, privateCIDRs, err := spec.cidrs()
	if err != nil {
		return nil, err
	}
	refs := &PrivateRefs{VpcCIDR: vpcCIDR, SubnetCIDR: privateCIDRs[0]}
	if refs.VpcID, err = ensureVPC(ctx, vpcp, spec, accountID, vpcCIDR); err != nil {
		return nil, err
	}
	if refs.SubnetID, err = ensureSubnet(ctx, vpcp, spec, accountID, refs.VpcID, refs.SubnetCIDR,
		spec.Roles().PrivateSubnet, spec.az(0)); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "systemvpc: private system VPC ready",
		"name", spec.Name, "managedBy", spec.ManagedBy, "vpc", refs.VpcID, "subnet", refs.SubnetID)
	return refs, nil
}
