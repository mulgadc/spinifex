package subnetgroup

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// AWS's cap on the subnets one group may hold.
const maxSubnets = 20

// Validates the requested subnets against the caller's own account and returns
// them with the AZ each subnet itself records. The describe is issued as the
// caller, so a subnet in another account simply does not come back and is
// reported as not found rather than leaked.
func (o *Owner) resolveSubnets(ctx context.Context, accountID string, requested []string) ([]Subnet, string, error) {
	if o.subnets == nil {
		return nil, "", awserrors.Errorf(awserrors.ErrorServerInternal, "RDS networking is not wired on this node")
	}
	switch {
	case len(requested) == 0:
		return nil, "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"SubnetIds must name at least one subnet")
	case len(requested) > maxSubnets:
		return nil, "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"a DB subnet group may hold at most %d subnets, got %d", maxSubnets, len(requested))
	}

	out, err := o.subnets.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: aws.StringSlice(requested),
	}, accountID)
	if err != nil {
		return nil, "", fmt.Errorf("rds: describe the subnets of the DB subnet group: %w", err)
	}

	found := map[string]*ec2.Subnet{}
	if out != nil {
		for _, subnet := range out.Subnets {
			if id := aws.StringValue(subnet.SubnetId); id != "" {
				found[id] = subnet
			}
		}
	}

	vpcID := ""
	subnets := make([]Subnet, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, id := range requested {
		if seen[id] {
			return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid, "subnet %s is named more than once", id)
		}
		seen[id] = true

		subnet, ok := found[id]
		if !ok {
			return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid,
				"subnet %s does not exist in this account", id)
		}
		subnetVPC := aws.StringValue(subnet.VpcId)
		if vpcID == "" {
			vpcID = subnetVPC
		}
		if subnetVPC != vpcID {
			return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid,
				"subnet %s is in VPC %s while the group's other subnets are in %s; "+
					"a DB subnet group must span one VPC", id, subnetVPC, vpcID)
		}
		// Read off the subnet rather than stamped from the platform's single zone,
		// so the response needs no change when V2 makes AZs real.
		subnets = append(subnets, Subnet{
			SubnetID:         id,
			AvailabilityZone: aws.StringValue(subnet.AvailabilityZone),
		})
	}
	if vpcID == "" {
		return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid, "the named subnets report no VPC")
	}
	return subnets, vpcID, nil
}
