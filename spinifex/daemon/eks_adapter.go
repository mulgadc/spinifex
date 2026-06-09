package daemon

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
)

// daemonEKSSubnetResolver adapts the daemon's VPCServiceImpl to the
// handlers_eks subnetVPCResolver-equivalent. handlers/eks declares the
// interface privately; the daemon needs an exported shape only because the
// constructor wires it via EKSServiceDeps.SubnetResolver = ...
//
// The translation is trivial: VPCService.GetSubnet returns a SubnetRecord
// with VpcId, which is the only field the EKS launcher needs.
type daemonEKSSubnetResolver struct {
	d *Daemon
}

func (a *daemonEKSSubnetResolver) GetSubnetVPC(accountID, subnetID string) (string, error) {
	if a.d == nil || a.d.vpcService == nil {
		return "", errors.New("eks: VPC service not initialized")
	}
	rec, err := a.d.vpcService.GetSubnet(accountID, subnetID)
	if err != nil {
		return "", err
	}
	return rec.VpcId, nil
}

func (a *daemonEKSSubnetResolver) GetVPCCIDR(accountID, vpcID string) (string, error) {
	if a.d == nil || a.d.vpcService == nil {
		return "", errors.New("eks: VPC service not initialized")
	}
	out, err := a.d.vpcService.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: aws.StringSlice([]string{vpcID}),
	}, accountID)
	if err != nil {
		return "", err
	}
	for _, v := range out.Vpcs {
		if v != nil && aws.StringValue(v.VpcId) == vpcID {
			return aws.StringValue(v.CidrBlock), nil
		}
	}
	return "", fmt.Errorf("eks: VPC %s not found", vpcID)
}

var _ handlers_eks.SubnetVPCResolver = (*daemonEKSSubnetResolver)(nil)
