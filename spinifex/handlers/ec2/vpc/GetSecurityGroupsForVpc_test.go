package handlers_ec2_vpc

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupIDsForVpc collects the ids one GetSecurityGroupsForVpc page reports.
func groupIDsForVpc(out *ec2.GetSecurityGroupsForVpcOutput) []string {
	ids := make([]string, 0, len(out.SecurityGroupForVpcs))
	for _, g := range out.SecurityGroupForVpcs {
		ids = append(ids, aws.StringValue(g.GroupId))
	}
	return ids
}

// describedGroupIDsForVpc is what DescribeSecurityGroups reports for the same
// VPC, so the narrower call can be held against the wider one rather than
// against a list the test itself built.
func describedGroupIDsForVpc(t *testing.T, svc *VPCServiceImpl, vpcID string) []string {
	t.Helper()
	out, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}}},
	}, testAccountID)
	require.NoError(t, err)
	ids := make([]string, 0, len(out.SecurityGroups))
	for _, sg := range out.SecurityGroups {
		ids = append(ids, aws.StringValue(sg.GroupId))
	}
	return ids
}

func TestGetSecurityGroupsForVpc_MatchesDescribeForSameVpc(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	web := createTestSG(t, svc, vpcID, "web-sg")
	db := createTestSG(t, svc, vpcID, "db-sg")

	out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	ids := groupIDsForVpc(out)
	assert.ElementsMatch(t, describedGroupIDsForVpc(t, svc, vpcID), ids)
	assert.Contains(t, ids, web)
	assert.Contains(t, ids, db)
	assert.Len(t, ids, 3, "the VPC's default group must be reported alongside the two created ones")
	assert.Nil(t, out.NextToken)

	for _, g := range out.SecurityGroupForVpcs {
		assert.Equal(t, vpcID, aws.StringValue(g.PrimaryVpcId))
		assert.Equal(t, testAccountID, aws.StringValue(g.OwnerId))
		assert.NotEmpty(t, aws.StringValue(g.GroupName))
	}
}

func TestGetSecurityGroupsForVpc_ExcludesOtherVpcInSameAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	otherVpcID := createTestVPC(t, svc, "10.1.0.0/16")
	mine := createTestSG(t, svc, vpcID, "mine-sg")
	theirs := createTestSG(t, svc, otherVpcID, "theirs-sg")

	out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	ids := groupIDsForVpc(out)
	assert.Contains(t, ids, mine)
	assert.NotContains(t, ids, theirs)
}

func TestGetSecurityGroupsForVpc_ExcludesOtherAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	const otherAccountID = "210987654321"

	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	mine := createTestSG(t, svc, vpcID, "mine-sg")

	otherVpc, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.2.0.0/16"),
	}, otherAccountID)
	require.NoError(t, err)
	otherSG, err := svc.CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("other-account-sg"),
		Description: aws.String("test sg"),
		VpcId:       otherVpc.Vpc.VpcId,
	}, otherAccountID)
	require.NoError(t, err)

	out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	ids := groupIDsForVpc(out)
	assert.Contains(t, ids, mine)
	assert.NotContains(t, ids, aws.StringValue(otherSG.GroupId))

	// The other account's own VPC is invisible to this caller too, rather than
	// returning that account's groups.
	_, err = svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId: otherVpc.Vpc.VpcId,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVpcIDNotFound, err.Error())
}

func TestGetSecurityGroupsForVpc_MissingVpcId(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)

	_, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestGetSecurityGroupsForVpc_UnknownVpcIsNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId: aws.String("vpc-00000000000000000"),
	}, testAccountID)
	require.Error(t, err, "an unknown VPC must not be reported as a VPC with no groups")
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidVpcIDNotFound, err.Error())
}

// A VPC whose only group is the default one still answers, so the not-found
// path above cannot be firing on an empty scan result.
func TestGetSecurityGroupsForVpc_DefaultGroupOnly(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId: aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupForVpcs, 1)
	assert.Equal(t, "default", aws.StringValue(out.SecurityGroupForVpcs[0].GroupName))
	assert.Nil(t, out.NextToken)
}

func TestGetSecurityGroupsForVpc_PagesEveryGroupOnce(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	expected := describedGroupIDsForVpc(t, svc, vpcID)
	for _, name := range []string{"sg-a", "sg-b", "sg-c", "sg-d", "sg-e", "sg-f", "sg-g"} {
		expected = append(expected, createTestSG(t, svc, vpcID, name))
	}

	var seen []string
	var token *string
	for page := 0; ; page++ {
		require.Less(t, page, 10, "paging did not terminate")

		out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
			VpcId:      aws.String(vpcID),
			MaxResults: aws.Int64(5),
			NextToken:  token,
		}, testAccountID)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(out.SecurityGroupForVpcs), 5)

		seen = append(seen, groupIDsForVpc(out)...)
		token = out.NextToken
		if token == nil {
			break
		}
	}

	assert.ElementsMatch(t, expected, seen)
	assert.Len(t, seen, len(expected), "a group was reported on more than one page")
}

func TestGetSecurityGroupsForVpc_MalformedNextToken(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId:     aws.String(vpcID),
		NextToken: aws.String("not-a-token"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestGetSecurityGroupsForVpc_Filters(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	web := createTestSG(t, svc, vpcID, "web-sg")
	createTestSG(t, svc, vpcID, "db-sg")

	out, err := svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId:   aws.String(vpcID),
		Filters: []*ec2.Filter{{Name: aws.String("group-name"), Values: []*string{aws.String("web-sg")}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{web}, groupIDsForVpc(out))

	// vpc-id is DescribeSecurityGroups' name for it; this model says
	// primary-vpc-id, and an unsupported filter must not be ignored.
	_, err = svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId:   aws.String(vpcID),
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}}},
	}, testAccountID)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	assert.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	assert.Contains(t, message, "vpc-id")

	out, err = svc.GetSecurityGroupsForVpc(context.Background(), &ec2.GetSecurityGroupsForVpcInput{
		VpcId:   aws.String(vpcID),
		Filters: []*ec2.Filter{{Name: aws.String("primary-vpc-id"), Values: []*string{aws.String(vpcID)}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroupForVpcs, 3)
}
