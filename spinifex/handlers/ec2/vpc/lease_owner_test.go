package handlers_ec2_vpc

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVPCExists(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	exists, err := svc.VPCExists(t.Context(), vpcID)
	require.NoError(t, err)
	assert.True(t, exists)
}

// A gw-lrp lease outliving its VPC is exactly the leak the reaper is for, so a
// deleted VPC has to read as absent.
func TestVPCExistsFalseAfterDelete(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)

	exists, err := svc.VPCExists(t.Context(), vpcID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestVPCExistsUnknownID(t *testing.T) {
	svc := setupTestVPCService(t)

	exists, err := svc.VPCExists(t.Context(), "vpc-missing")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestENIExists(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)

	exists, err := svc.ENIExists(t.Context(), eniID)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestENIExistsUnknownID(t *testing.T) {
	svc := setupTestVPCService(t)

	exists, err := svc.ENIExists(t.Context(), "eni-missing")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestLeaseOwnerLookupsRejectEmptyIDs(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.VPCExists(t.Context(), "")
	require.Error(t, err)
	_, err = svc.ENIExists(t.Context(), "")
	require.Error(t, err)
}
