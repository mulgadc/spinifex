package handlers_ec2_vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureDefaultVPC_PublicIPMappingDisabled(t *testing.T) {
	svc := setupTestVPCService(t)
	svc.SetDefaultPublicIPMapping(false)

	_, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	subDesc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.False(t, *subDesc.Subnets[0].MapPublicIpOnLaunch,
		"non-pool external modes must not seed public-IP mapping")
}

func TestEnsureDefaultVPC_PublicIPMappingDefaultOn(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)

	subDesc, err := svc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, subDesc.Subnets, 1)
	assert.True(t, *subDesc.Subnets[0].MapPublicIpOnLaunch)
}
