package handlers_rds

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// The security-group surface the system ENI's group is ensured through. Narrow
// on purpose: this never authorizes a rule, so it holds no authorize verb.
type systemSecurityGroupProvisioner interface {
	CreateSecurityGroup(ctx context.Context, input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error)
	DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
}

// One shared RDS system VPC per region holds every DB VM's primary NIC. DB
// instances are isolated by their own customer ENI and security group, so a VPC
// per instance would add a NAT gateway and EIP without adding isolation.
//
// Own tag keys and address space, so no EKS teardown or orphan reaper can reach
// it even though it shares the EKS control-plane VPC's builder and topology.
const (
	rdsSystemVPCTagKey     = "spinifex:rds-system-vpc"
	rdsSystemVPCRoleTagKey = "spinifex:rds-role"
	rdsSystemVPCRolePrefix = "rds"
)

// Seeds both the owner tag and the deterministic address hash, so it must stay
// stable across releases.
func SystemVPCName(region string) string {
	return "rds-system-" + region
}

// One group per region, shared by every DB VM's system ENI. The name is the
// lookup key, so it must stay stable across releases too.
func SystemSecurityGroupName(region string) string {
	return "rds-system-" + region
}

// cfg supplies the operator-overridable address space and subnet count; a nil
// or unset cfg falls back to the defaults.
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

// Idempotent. The private subnet the DB VMs sit in routes 0.0.0.0/0 to the
// VPC's NAT gateway, which is the agent's egress to the gateway wherever no
// management bridge exists. On a formed deployment one always does, and the
// agent reaches the gateway over it instead — so this path carries almost no
// required traffic.
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

// The security group every DB VM's system ENI carries, found by name or created.
// Idempotent, and shared across the region: nothing deletes it per instance.
//
// No ingress rules are authorized, and none are needed. Nothing in the platform
// connects to a DB VM's system ENI — the agent long-polls outbound, the control
// plane never dials the engine, and the gateway is reached over the management
// bridge. The ACLs are allow-related, so an agent-initiated connection's return
// traffic is matched by conntrack rather than by an ingress rule.
//
// The whole of the fix is not landing in the VPC's default group, whose one
// ingress rule admits every other member of itself — which is every DB VM in
// the deployment, across every account.
func EnsureSystemSecurityGroup(ctx context.Context, sgs systemSecurityGroupProvisioner, accountID, region, vpcID string) (string, error) {
	if region == "" {
		return "", errors.New("rds: EnsureSystemSecurityGroup empty region")
	}
	if vpcID == "" {
		return "", errors.New("rds: EnsureSystemSecurityGroup empty vpc id")
	}
	name := SystemSecurityGroupName(region)

	out, err := sgs.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: aws.StringSlice([]string{name})},
			{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})},
		},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("rds: describe the system security group %s: %w", name, err)
	}
	var matches []*ec2.SecurityGroup
	if out != nil {
		for _, group := range out.SecurityGroups {
			if group == nil || aws.StringValue(group.GroupId) == "" {
				return "", fmt.Errorf("rds: describe the system security group %s: group has no id", name)
			}
			matches = append(matches, group)
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("rds: describe the system security group %s: found %d groups, want exactly one", name, len(matches))
	}
	if len(matches) == 1 {
		group := matches[0]
		id := aws.StringValue(group.GroupId)
		if len(group.IpPermissions) != 0 {
			return "", fmt.Errorf("rds: system security group %s (%s) has ingress rules; refusing to attach DB system NICs", name, id)
		}
		return id, nil
	}

	created, err := sgs.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String("RDS management NIC security group; no ingress"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("security-group"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
				{Key: aws.String(rdsSystemVPCTagKey), Value: aws.String(SystemVPCName(region))},
				{Key: aws.String(rdsSystemVPCRoleTagKey), Value: aws.String(rdsSystemVPCRolePrefix)},
			},
		}},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("rds: create the system security group %s: %w", name, err)
	}
	if created == nil || aws.StringValue(created.GroupId) == "" {
		return "", fmt.Errorf("rds: create the system security group %s: no group id returned", name)
	}
	return aws.StringValue(created.GroupId), nil
}
