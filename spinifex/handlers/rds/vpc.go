package handlers_rds

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// One shared RDS system VPC per region holds every DB VM's primary NIC. DB
// instances are isolated by their own customer-facing ENI and security group,
// so a VPC per instance would add a NAT gateway and EIP without adding
// isolation.
//
// It is the EKS control-plane VPC's sibling — same builder and topology — but
// with RDS's own tag keys and address space, so no EKS teardown or orphan
// reaper can reach it.
const (
	// rdsSystemVPCTagKey holds the system VPC's name on every resource in it.
	rdsSystemVPCTagKey = "spinifex:rds-system-vpc"

	// rdsSystemVPCRoleTagKey distinguishes the resources within one system VPC.
	rdsSystemVPCRoleTagKey = "spinifex:rds-role"

	// rdsSystemVPCRolePrefix namespaces those role values ("rds-vpc", …).
	rdsSystemVPCRolePrefix = "rds"
)

// SystemVPCName is the system VPC's name for region. It seeds both the owner tag
// and the deterministic address hash, so it must stay stable across releases.
func SystemVPCName(region string) string {
	return "rds-system-" + region
}

// SystemVPCSpec is the build spec for the region's shared RDS system VPC. cfg
// supplies the operator-overridable address space and subnet count; a nil or
// unset cfg falls back to the defaults.
func SystemVPCSpec(cfg *config.RDSConfig, region string) handlers_systemvpc.Spec {
	supernet := config.RDSDefaultSystemVPCSupernet
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
			ManagedBy:   tags.ManagedByRDS,
			OwnerTagKey: rdsSystemVPCTagKey,
			RoleTagKey:  rdsSystemVPCRoleTagKey,
		},
		Region:         region,
		RolePrefix:     rdsSystemVPCRolePrefix,
		Supernet:       supernet,
		PrivateSubnets: privateSubnets,
	}
}

// EnsureSystemVPC idempotently builds the region's shared RDS system VPC under
// accountID, the system account that owns every DB VM. The private subnet its
// VMs sit in routes 0.0.0.0/0 to the VPC's NAT gateway, which is the agent's
// only egress to the gateway.
func EnsureSystemVPC(ctx context.Context, deps handlers_systemvpc.Deps, cfg *config.RDSConfig, accountID, region string) (*handlers_systemvpc.Refs, error) {
	if region == "" {
		return nil, errors.New("rds: EnsureSystemVPC empty region")
	}
	refs, err := handlers_systemvpc.Ensure(ctx, deps, SystemVPCSpec(cfg, region), accountID)
	if err != nil {
		return nil, fmt.Errorf("rds: ensure system VPC for %s: %w", region, err)
	}
	if len(refs.PrivateSubnetIDs) == 0 {
		return nil, fmt.Errorf("rds: system VPC %s has no private subnet to place DB VMs in", refs.VpcID)
	}
	return refs, nil
}
