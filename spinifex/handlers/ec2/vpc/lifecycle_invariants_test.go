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

// TestRLC_ENINonDeadlock enforces ADR-0003 §2 (break the un-terminable-ENI
// deadlock). The public DeleteNetworkInterface guard must keep rejecting an
// in-use ENI — that protects an ENI a *different* live instance holds — while
// ForceDeleteInstanceENI must delete the owning instance's in-use ENI anyway.
// Without the force path a failed DetachENI CAS leaves the ENI stuck "in-use"
// and the in-use guard then blocks delete forever, so the instance can never
// finish terminating.
func TestRLC_ENINonDeadlock(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	// Simulate the deadlock precondition: the ENI is "in-use" because a prior
	// DetachENI never flipped it back to "available".
	_, err := svc.AttachENI(testAccountID, eniId, "i-deadlock", 0)
	require.NoError(t, err)

	// The guard stays intact: a plain delete of an in-use ENI is still rejected.
	_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniId),
	}, testAccountID)
	assert.ErrorContainsf(t, err, "InvalidNetworkInterface.InUse",
		"ADR-0003 §2: the in-use guard must still protect an ENI held by a different live instance")

	// The owning instance's force teardown must break the deadlock.
	require.NoErrorf(t, svc.ForceDeleteInstanceENI(testAccountID, eniId),
		"ADR-0003 §2: ForceDeleteInstanceENI must delete the owning instance's in-use ENI to break the un-terminable-ENI deadlock")

	_, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")

	// Idempotent: forcing teardown of an already-gone ENI is success.
	require.NoErrorf(t, svc.ForceDeleteInstanceENI(testAccountID, eniId),
		"ADR-0003 §2 / RLC rule #1: ForceDeleteInstanceENI on an absent ENI must be idempotent success")
}
