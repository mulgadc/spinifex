//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/require"
)

// groupDescribeRegionsPolicy is a single-action grant, so the enforcement
// positive case proves a specific group-attached (or group-inline) policy was
// resolved and evaluated - not a blanket allow.
const groupDescribeRegionsPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`

// TestIAMGroupEnforcement proves a policy attached to a group actually grants
// its permission to every member: a user with its own access key is denied a
// guarded action, allowed once a policy granting that action is attached to a
// group the user joins (policies resolved per request), and denied again
// after leaving the group. ec2:DescribeRegions is dispatched entirely inside
// the gateway, so this needs no NATS stub.
func TestIAMGroupEnforcement(t *testing.T) {
	gw := StartGateway(t)

	userOut, err := gw.IAMClient(t).CreateUser(&iam.CreateUserInput{UserName: aws.String("grp-enf-user")})
	require.NoError(t, err, "create-user")
	key, err := gw.IAMClient(t).CreateAccessKey(&iam.CreateAccessKeyInput{UserName: userOut.User.UserName})
	require.NoError(t, err, "create-access-key")
	memberCli := gw.ClientsWithCreds(t,
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey))

	// No policy anywhere yet: the active key authenticates, then default-denies.
	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	// Group + policy granting ec2:DescribeRegions, then join the user.
	groupOut, err := gw.IAMClient(t).CreateGroup(&iam.CreateGroupInput{GroupName: aws.String("grp-enforce")})
	require.NoError(t, err, "create-group")

	policyOut, err := gw.IAMClient(t).CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String("grp-enf-describe-regions"),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	require.NoError(t, err, "create-policy")
	_, err = gw.IAMClient(t).AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: groupOut.Group.GroupName,
		PolicyArn: policyOut.Policy.Arn,
	})
	require.NoError(t, err, "attach-group-policy")

	_, err = gw.IAMClient(t).AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: groupOut.Group.GroupName,
		UserName:  userOut.User.UserName,
	})
	require.NoError(t, err, "add-user-to-group")

	// Same live credentials are now allowed - the grant flows through the group.
	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the group grants it")

	// Leave the group and the inherited grant must evaporate.
	_, err = gw.IAMClient(t).RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
		GroupName: groupOut.Group.GroupName,
		UserName:  userOut.User.UserName,
	})
	require.NoError(t, err, "remove-user-from-group")

	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")
}

// TestIAMGroupInlineEnforcement proves an inline policy embedded in a group
// grants its permission to every member exactly as a managed attachment
// does: denied with no grant, allowed once the inline policy is put and the
// user joins, denied again after the inline policy is deleted - with the
// user still a member, isolating the inline document as the sole grant
// source. ec2:DescribeRegions needs no NATS stub.
func TestIAMGroupInlineEnforcement(t *testing.T) {
	gw := StartGateway(t)

	userOut, err := gw.IAMClient(t).CreateUser(&iam.CreateUserInput{UserName: aws.String("grp-inl-enf-user")})
	require.NoError(t, err, "create-user")
	key, err := gw.IAMClient(t).CreateAccessKey(&iam.CreateAccessKeyInput{UserName: userOut.User.UserName})
	require.NoError(t, err, "create-access-key")
	memberCli := gw.ClientsWithCreds(t,
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey))

	// No grant anywhere yet: the active key authenticates, then default-denies.
	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	// Group with an inline policy granting ec2:DescribeRegions, then join the user.
	groupOut, err := gw.IAMClient(t).CreateGroup(&iam.CreateGroupInput{GroupName: aws.String("grp-inline-enforce")})
	require.NoError(t, err, "create-group")

	const inlinePolicyName = "grp-inl-enf-describe-regions"
	_, err = gw.IAMClient(t).PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      groupOut.Group.GroupName,
		PolicyName:     aws.String(inlinePolicyName),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	require.NoError(t, err, "put-group-policy")

	_, err = gw.IAMClient(t).AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: groupOut.Group.GroupName,
		UserName:  userOut.User.UserName,
	})
	require.NoError(t, err, "add-user-to-group")

	// Same live credentials are now allowed - the inline grant flows through the group.
	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the group inline policy grants it")

	// Delete the inline policy while the user is still a member - the grant
	// must evaporate, isolating the inline document as the sole grant source.
	_, err = gw.IAMClient(t).DeleteGroupPolicy(&iam.DeleteGroupPolicyInput{
		GroupName:  groupOut.Group.GroupName,
		PolicyName: aws.String(inlinePolicyName),
	})
	require.NoError(t, err, "delete-group-policy")

	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")
}

// TestIAMUserInlineEnforcement proves the user-inline linchpin: a policy
// embedded directly in a user record grants that user its permission with no
// group or managed attachment involved. A user with its own access key is
// denied a guarded action; once an inline policy granting that action is put
// on the user, the same live credentials are allowed (policies resolved per
// request); after the inline policy is deleted the action is denied again,
// isolating the user's own inline document as the sole grant source.
// ec2:DescribeRegions needs no NATS stub.
func TestIAMUserInlineEnforcement(t *testing.T) {
	gw := StartGateway(t)

	userOut, err := gw.IAMClient(t).CreateUser(&iam.CreateUserInput{UserName: aws.String("usr-inl-enf-user")})
	require.NoError(t, err, "create-user")
	key, err := gw.IAMClient(t).CreateAccessKey(&iam.CreateAccessKeyInput{UserName: userOut.User.UserName})
	require.NoError(t, err, "create-access-key")
	userCli := gw.ClientsWithCreds(t,
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey))

	// No grant anywhere yet: the active key authenticates, then default-denies.
	_, err = userCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	// Put an inline policy granting ec2:DescribeRegions directly on the user.
	const inlinePolicyName = "usr-inl-enf-describe-regions"
	_, err = gw.IAMClient(t).PutUserPolicy(&iam.PutUserPolicyInput{
		UserName:       userOut.User.UserName,
		PolicyName:     aws.String(inlinePolicyName),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	require.NoError(t, err, "put-user-policy")

	// Same live credentials are now allowed - the user's own inline grant applies.
	_, err = userCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the user inline policy grants it")

	// Delete the inline policy - the grant must evaporate, isolating the
	// inline document as the sole grant source.
	_, err = gw.IAMClient(t).DeleteUserPolicy(&iam.DeleteUserPolicyInput{
		UserName:   userOut.User.UserName,
		PolicyName: aws.String(inlinePolicyName),
	})
	require.NoError(t, err, "delete-user-policy")

	_, err = userCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")
}
