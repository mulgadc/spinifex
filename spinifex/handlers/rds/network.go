package handlers_rds

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// The customer-account VPC surface CreateDBInstance reads to place the endpoint
// ENI. Read-only: the ENI itself is created through the launch deps.
type networkResolver interface {
	DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error)
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
	DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
}

// Where the customer-facing ENI lands.
type endpointPlacement struct {
	VpcID            string
	SubnetID         string
	SecurityGroupIDs []string
}

// A named DB subnet group decides the VPC and the subnet; an unnamed one falls
// back to the account's default VPC, mirroring AWS's own behaviour. The security
// groups are validated against whichever VPC that resolved to, because an ENI
// cannot carry a group from another one.
func (s *Service) resolvePlacement(ctx context.Context, kv jetstream.KeyValue, accountID string, req *validatedCreate) (*endpointPlacement, error) {
	if s.deps.Network == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "RDS networking is not wired on this node")
	}

	var vpcID, subnetID string
	if req.DBSubnetGroupName != "" {
		group, _, err := getDBSubnetGroup(ctx, kv, req.DBSubnetGroupName)
		if err != nil {
			return nil, err
		}
		subnetID, err = subnetFromGroup(group)
		if err != nil {
			return nil, err
		}
		vpcID = group.VpcID
	} else {
		var err error
		if vpcID, err = s.defaultVPCID(ctx, accountID); err != nil {
			return nil, err
		}
		if subnetID, err = s.firstSubnetID(ctx, accountID, vpcID); err != nil {
			return nil, err
		}
	}

	groupIDs, err := s.resolveSecurityGroups(ctx, accountID, vpcID, req.SecurityGroupIDs)
	if err != nil {
		return nil, err
	}
	return &endpointPlacement{VpcID: vpcID, SubnetID: subnetID, SecurityGroupIDs: groupIDs}, nil
}

// The subnet a group places an instance in. Sorted rather than first-listed, so
// two instances created against the same group land the same way regardless of
// the order the subnets were supplied in. Single-AZ makes the choice arbitrary
// today; when V2 makes AZs real this is the one function that changes.
func subnetFromGroup(group *DBSubnetGroupRecord) (string, error) {
	ids := make([]string, 0, len(group.Subnets))
	for _, subnet := range group.Subnets {
		if subnet.SubnetID != "" {
			ids = append(ids, subnet.SubnetID)
		}
	}
	if len(ids) == 0 {
		return "", awserrors.Errorf(awserrors.ErrorDBInvalidVPCNetworkState,
			"DB subnet group %s holds no subnet to place the DB endpoint in", group.Name)
	}
	slices.Sort(ids)
	return ids[0], nil
}

func (s *Service) defaultVPCID(ctx context.Context, accountID string) (string, error) {
	out, err := s.deps.Network.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{{Name: aws.String("is-default"), Values: aws.StringSlice([]string{"true"})}},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("rds: describe default VPC: %w", err)
	}
	if out != nil {
		for _, vpc := range out.Vpcs {
			if id := aws.StringValue(vpc.VpcId); id != "" {
				return id, nil
			}
		}
	}
	return "", awserrors.Errorf(awserrors.ErrorDBInvalidVPCNetworkState,
		"no default VPC in this account to place the DB endpoint in")
}

// Sorted so a repeated create in the same account is placed deterministically
// rather than wherever the describe happened to order its results.
func (s *Service) firstSubnetID(ctx context.Context, accountID, vpcID string) (string, error) {
	out, err := s.deps.Network.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})}},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("rds: describe subnets in %s: %w", vpcID, err)
	}
	var ids []string
	if out != nil {
		for _, subnet := range out.Subnets {
			if id := aws.StringValue(subnet.SubnetId); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return "", awserrors.Errorf(awserrors.ErrorDBInvalidVPCNetworkState,
			"default VPC %s has no subnet to place the DB endpoint in", vpcID)
	}
	slices.Sort(ids)
	return ids[0], nil
}

// An unset list takes the VPC's default group, matching AWS. A supplied one is
// checked against the placement VPC, because an ENI cannot carry a group from
// another VPC and the launch would otherwise fail after the record exists.
func (s *Service) resolveSecurityGroups(ctx context.Context, accountID, vpcID string, requested []string) ([]string, error) {
	out, err := s.deps.Network.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})}},
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("rds: describe security groups in %s: %w", vpcID, err)
	}

	inVPC := map[string]string{}
	defaultGroupID := ""
	if out != nil {
		for _, group := range out.SecurityGroups {
			id := aws.StringValue(group.GroupId)
			if id == "" {
				continue
			}
			name := aws.StringValue(group.GroupName)
			inVPC[id] = name
			if name == "default" {
				defaultGroupID = id
			}
		}
	}

	if len(requested) == 0 {
		if defaultGroupID == "" {
			return nil, awserrors.Errorf(awserrors.ErrorDBInvalidVPCNetworkState,
				"VPC %s has no default security group", vpcID)
		}
		return []string{defaultGroupID}, nil
	}
	for _, id := range requested {
		if _, ok := inVPC[id]; !ok {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"security group %s is not in VPC %s", id, vpcID)
		}
	}
	return requested, nil
}
