package vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/require"
)

func TestGroupLookup_GroupVPC(t *testing.T) {
	ctx := t.Context()
	svc := setupTestVPCService(t)
	vpcOut, err := svc.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.90.0.0/16")}, testAccountID)
	require.NoError(t, err)
	sgOut, err := svc.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: aws.String("lookup"), Description: aws.String("lookup"), VpcId: vpcOut.Vpc.VpcId,
	}, testAccountID)
	require.NoError(t, err)
	groupID := aws.StringValue(sgOut.GroupId)
	lookup := NewGroupLookup(svc.sgKV)

	vpcID, found, err := lookup.GroupVPC(ctx, testAccountID, groupID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, aws.StringValue(vpcOut.Vpc.VpcId), vpcID)

	_, found, err = lookup.GroupVPC(ctx, "000000000002", groupID)
	require.NoError(t, err)
	require.False(t, found, "a group of another account must read as absent")

	_, found, err = lookup.GroupVPC(ctx, testAccountID, "sg-00000000000000000")
	require.NoError(t, err)
	require.False(t, found)

	_, found, err = NewGroupLookup(failGetKV{KeyValue: svc.sgKV}).GroupVPC(ctx, testAccountID, groupID)
	require.ErrorContains(t, err, "simulated KV read failure")
	require.False(t, found, "a failed read must not read as absent")
}
