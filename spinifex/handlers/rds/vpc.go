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
// instances are isolated from each other by their own customer-facing ENI and
// security group, so a VPC per instance would add a NAT gateway and an EIP per
// database without adding isolation.
//
// The VPC is the EKS control-plane VPC's sibling — same builder, same
// public/private + NAT-gateway topology — but with RDS's own tag keys and its
// own address space, so no EKS teardown or orphan reaper can reach it and no
// address ambiguity survives.
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

// EnsureSystemVPC builds (idempotently) the region's shared RDS system VPC under
// accountID — the system account, which owns every DB VM. A second call with the
// same region returns the same refs and creates nothing.
//
// The private subnet its VMs sit in routes 0.0.0.0/0 to the VPC's NAT gateway,
// giving the in-guest rds-agent the same egress the EKS control-plane VMs have.
// The customer's DB subnet needs no equivalent: the customer-facing ENI is
// ingress-only, which is why the DB VM is dual-NIC in the first place.
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
