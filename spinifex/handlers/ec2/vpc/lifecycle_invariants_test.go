package handlers_ec2_vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_VPCDeleteNotFoundOnAbsent enforces the Common Resource Lifecycle
// Contract rule #1 (AWS-faithful delete, per-service): the EC2/VPC delete API
// returns the service's canonical InvalidX.NotFound for an absent resource —
// not success. Idempotent convergence belongs to destroy orchestration, which
// tolerates NotFound via awserrors.IsNotFound; the public API must stay AWS
// compatible so SDK callers and account-scoping checks see the real error.
// Every VPC-family delete endpoint must have a case here.
func TestRLC1_VPCDeleteNotFoundOnAbsent(t *testing.T) {
	cases := []struct {
		name    string
		wantErr string
		call    func(svc *VPCServiceImpl) (any, error)
	}{
		{"DeleteVpc", awserrors.ErrorInvalidVpcIDNotFound, func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String("vpc-absent00000000")}, testAccountID)
		}},
		{"DeleteSubnet", awserrors.ErrorInvalidSubnetIDNotFound, func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String("subnet-absent0000")}, testAccountID)
		}},
		{"DeleteSecurityGroup", awserrors.ErrorInvalidGroupNotFound, func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String("sg-absent000000000")}, testAccountID)
		}},
		{"DeleteNetworkInterface", awserrors.ErrorInvalidNetworkInterfaceIDNotFound, func(svc *VPCServiceImpl) (any, error) {
			return svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String("eni-absent00000000")}, testAccountID)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := setupTestVPCServiceWithNC(t)
			_, err := tc.call(svc)
			require.Errorf(t, err, "%s on an absent resource must return %s, not success (RLC rule #1 AWS-faithful delete): destroy orchestration tolerates NotFound, the API must not", tc.name, tc.wantErr)
			assert.ErrorContainsf(t, err, tc.wantErr, "%s on an absent resource must return the canonical %s (RLC rule #1)", tc.name, tc.wantErr)
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

// TestRLC3_VPCFamilyDeleteGuardsLiveDependents enforces the Common Resource
// Lifecycle Contract rule #3 (live-only dependency guards): a VPC-family delete
// must return DependencyViolation while a *live* dependent remains, but a
// detached/available (orphan) child must never pin its parent undeletable —
// otherwise tofu destroy deadlocks on a child the GC backstop already reaps.
func TestRLC3_VPCFamilyDeleteGuardsLiveDependents(t *testing.T) {
	t.Run("DeleteSubnet blocked by an attached ENI", func(t *testing.T) {
		svc := setupTestVPCService(t)
		vpcId := createTestVPC(t, svc, "10.0.0.0/16")
		subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
		eniId := createTestENI(t, svc, subnetId)
		_, err := svc.AttachENI(testAccountID, eniId, "i-resident000000", 0)
		require.NoError(t, err)

		_, err = svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetId)}, testAccountID)
		assert.ErrorContainsf(t, err, awserrors.ErrorDependencyViolation,
			"ADR-0004 §2: DeleteSubnet must return DependencyViolation while a live ENI attachment (a resident instance) remains (rule #3)")
	})

	t.Run("DeleteSubnet allowed with only an available ENI", func(t *testing.T) {
		svc := setupTestVPCService(t)
		vpcId := createTestVPC(t, svc, "10.0.0.0/16")
		subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
		_ = createTestENI(t, svc, subnetId) // available, never attached

		_, err := svc.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(subnetId)}, testAccountID)
		require.NoErrorf(t, err,
			"ADR-0004 §2/rule #3: an available (orphan) ENI must not pin its subnet — it is itself deletable and GC-reaped")
	})

	t.Run("DeleteNetworkInterface blocked while attached", func(t *testing.T) {
		svc := setupTestVPCService(t)
		vpcId := createTestVPC(t, svc, "10.0.0.0/16")
		subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
		eniId := createTestENI(t, svc, subnetId)
		_, err := svc.AttachENI(testAccountID, eniId, "i-live0000000000", 0)
		require.NoError(t, err)

		_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniId)}, testAccountID)
		assert.ErrorContainsf(t, err, awserrors.ErrorInvalidNetworkInterfaceInUse,
			"ADR-0004 §2: a plain DeleteNetworkInterface must reject an ENI attached to a live instance")
	})

	t.Run("DeleteNetworkInterface allowed once detached", func(t *testing.T) {
		svc := setupTestVPCService(t)
		vpcId := createTestVPC(t, svc, "10.0.0.0/16")
		subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
		eniId := createTestENI(t, svc, subnetId)
		_, err := svc.AttachENI(testAccountID, eniId, "i-gone0000000000", 0)
		require.NoError(t, err)
		require.NoError(t, svc.DetachENI(testAccountID, eniId))

		_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniId)}, testAccountID)
		require.NoErrorf(t, err,
			"ADR-0004 §2/rule #3: an ENI whose instance is gone (detached) must be deletable")
	})
}
