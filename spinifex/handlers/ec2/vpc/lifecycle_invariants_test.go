package handlers_ec2_vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_VPCDeleteIdempotentOnAbsent enforces the Common Resource Lifecycle
// Contract rule #1 (idempotent delete): deleting an absent VPC-family resource
// is success, not NotFound, so tofu destroy retries and double-targeted graph
// nodes converge. Every VPC-family delete endpoint must have a case here — a
// missing case is an idempotency gap.
func TestRLC1_VPCDeleteIdempotentOnAbsent(t *testing.T) {
	cases := []struct {
		name string
		call func(svc *VPCServiceImpl) (any, error)
	}{
		{"DeleteVpc", func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String("vpc-absent00000000")}, testAccountID)
		}},
		{"DeleteSubnet", func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String("subnet-absent0000")}, testAccountID)
		}},
		{"DeleteSecurityGroup", func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String("sg-absent000000000")}, testAccountID)
		}},
		{"DeleteNetworkInterface", func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String("eni-absent00000000")}, testAccountID)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := setupTestVPCServiceWithNC(t)
			out, err := tc.call(svc)
			require.NoErrorf(t, err, "%s on an absent resource must return success, not NotFound (RLC rule #1 idempotent delete): return an empty output on nats.ErrKeyNotFound", tc.name)
			assert.NotNilf(t, out, "%s must return a non-nil output on absent (RLC rule #1)", tc.name)
		})
	}
}
