package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// One shared Bedrock system VPC per region holds every serving VM's primary
// NIC. A serving VM has no customer ENI to isolate — inference traffic and
// the readiness probe both dial the ENI's private IP directly — so a VPC per
// endpoint would add a NAT gateway and EIP without adding isolation.
//
// Own tag keys and address space, so no EKS/RDS teardown or orphan reaper can
// reach it even though it shares their VPC builder and topology.
const (
	bedrockSystemVPCTagKey     = "spinifex:bedrock-system-vpc"
	bedrockSystemVPCRoleTagKey = "spinifex:bedrock-role"
	bedrockSystemVPCRolePrefix = "bedrock"
)

// SystemVPCName is stable across releases: it seeds both the owner tag and
// the deterministic address hash.
func SystemVPCName(region string) string {
	return "bedrock-system-" + region
}

// cfg supplies the operator-overridable address space and subnet count; a nil
// or unset cfg falls back to the defaults.
func SystemVPCSpec(cfg *config.BedrockConfig, region string) handlers_systemvpc.Spec {
	supernet := config.BedrockDefaultSystemVPCSupernet
	privateSubnets := 1
	if cfg != nil {
		if cfg.SystemVPCSupernet != "" {
			supernet = cfg.SystemVPCSupernet
		}
		if cfg.SystemVPCPrivateSubnets > 0 {
			privateSubnets = cfg.SystemVPCPrivateSubnets
		}
	}
	return handlers_systemvpc.Spec{
		Owner: handlers_systemvpc.Owner{
			Name:        SystemVPCName(region),
			ManagedBy:   tags.ManagedByBedrock,
			OwnerTagKey: bedrockSystemVPCTagKey,
			RoleTagKey:  bedrockSystemVPCRoleTagKey,
		},
		Region:         region,
		RolePrefix:     bedrockSystemVPCRolePrefix,
		Supernet:       supernet,
		PrivateSubnets: privateSubnets,
	}
}

// EnsureSystemVPC is idempotent. The private subnet the serving VMs sit in
// routes 0.0.0.0/0 to the VPC's NAT gateway, which is the guest's only egress
// to fetch anything the baked AMI doesn't already carry.
func EnsureSystemVPC(ctx context.Context, deps handlers_systemvpc.Deps, cfg *config.BedrockConfig, accountID, region string) (*handlers_systemvpc.Refs, error) {
	if region == "" {
		return nil, errors.New("bedrock: EnsureSystemVPC empty region")
	}
	refs, err := handlers_systemvpc.Ensure(ctx, deps, SystemVPCSpec(cfg, region), accountID)
	if err != nil {
		return nil, fmt.Errorf("bedrock: ensure system VPC for %s: %w", region, err)
	}
	if len(refs.PrivateSubnetIDs) == 0 {
		return nil, fmt.Errorf("bedrock: system VPC %s has no private subnet to place serving VMs in", refs.VpcID)
	}
	// The two are built in lockstep, and callers index both by the same k to
	// pair a subnet with its prefix length.
	if len(refs.PrivateSubnetCIDRs) != len(refs.PrivateSubnetIDs) {
		return nil, fmt.Errorf("bedrock: system VPC %s returned %d private subnets but %d CIDRs",
			refs.VpcID, len(refs.PrivateSubnetIDs), len(refs.PrivateSubnetCIDRs))
	}
	return refs, nil
}
