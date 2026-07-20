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

// Dedicated identifiers for the Groups lifecycle test, distinct from the
// enforcement tests above so a future case reordering can't collide.
const (
	grpLifecycleGroup  = "grp-developers"
	grpLifecycleUser   = "grp-lc-user"
	grpLifecyclePolicy = "grp-lc-describe-regions"
	grpLifecycleInline = "grp-lc-inline-describe-regions"
)

// TestIAMGroupsLifecycle exercises the full group surface: CRUD, membership,
// group-policy attachment, inline group policies (put/get/list), the listing
// reverse-lookups, and every deletion guard (delete-group while attached,
// while inline-policied, while non-empty, delete-user while in a group), then
// a clean teardown. No NATS stub needed — groups are pure IAM state.
func TestIAMGroupsLifecycle(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)
	adminAccount := gw.AccountID
	groupARN := iamGroupARN(adminAccount, grpLifecycleGroup)
	policyARN := iamPolicyARN(adminAccount, grpLifecyclePolicy)

	// CreateGroup — happy path; assert ARN + AWS group-id prefix.
	createOut, err := iamCli.CreateGroup(&iam.CreateGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "create-group")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(createOut.Group.GroupName))
	require.Equal(t, groupARN, aws.StringValue(createOut.Group.Arn),
		"group ARN must follow arn:aws:iam::<acct>:group/<name>")
	require.True(t, len(aws.StringValue(createOut.Group.GroupId)) > 4 &&
		aws.StringValue(createOut.Group.GroupId)[:4] == "AGPA",
		"group id must carry the AWS AGPA prefix, got %q", aws.StringValue(createOut.Group.GroupId))

	// Duplicate.
	_, err = iamCli.CreateGroup(&iam.CreateGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	requireAWSErrorCode(t, err, "EntityAlreadyExists")

	// GetGroup — fresh group has an empty (non-nil) member list.
	got, err := iamCli.GetGroup(&iam.GetGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "get-group")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(got.Group.GroupName))
	require.Empty(t, got.Users, "new group must have zero members")

	_, err = iamCli.GetGroup(&iam.GetGroupInput{GroupName: aws.String("ghost-group")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// ListGroups + PathPrefix scan.
	listed, err := iamCli.ListGroups(&iam.ListGroupsInput{})
	require.NoError(t, err, "list-groups")
	require.GreaterOrEqual(t, len(listed.Groups), 1, "expected >= 1 group, got %d", len(listed.Groups))

	pp, err := iamCli.ListGroups(&iam.ListGroupsInput{PathPrefix: aws.String("/")})
	require.NoError(t, err, "list-groups --path-prefix /")
	require.GreaterOrEqual(t, len(pp.Groups), 1, "path-prefix / must surface groups at /")

	none, err := iamCli.ListGroups(&iam.ListGroupsInput{PathPrefix: aws.String("/nonexistent/")})
	require.NoError(t, err, "list-groups --path-prefix /nonexistent/")
	require.Empty(t, none.Groups, "no group lives under /nonexistent/")

	// Member for the membership + guard sub-steps.
	_, err = iamCli.CreateUser(&iam.CreateUserInput{UserName: aws.String(grpLifecycleUser)})
	require.NoError(t, err, "create-user")

	// remove-user-from-group on a non-member surfaces NoSuchEntity (must not no-op).
	_, err = iamCli.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// Ghost-entity binds → NoSuchEntity. Group is validated before the user.
	_, err = iamCli.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String("ghost-group"),
		UserName:  aws.String(grpLifecycleUser),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")
	_, err = iamCli.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String("ghost-user"),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// AddUserToGroup — idempotent re-add must not duplicate the membership.
	_, err = iamCli.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "add-user-to-group")

	_, err = iamCli.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "idempotent re-add")

	// ListGroupsForUser — reverse lookup sees exactly the one group.
	forUser, err := iamCli.ListGroupsForUser(&iam.ListGroupsForUserInput{UserName: aws.String(grpLifecycleUser)})
	require.NoError(t, err, "list-groups-for-user")
	require.Len(t, forUser.Groups, 1, "user should belong to exactly one group")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(forUser.Groups[0].GroupName))

	// GetGroup — member now visible from the group side.
	got, err = iamCli.GetGroup(&iam.GetGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "get-group after add")
	require.Len(t, got.Users, 1, "group should list exactly one member")
	require.Equal(t, grpLifecycleUser, aws.StringValue(got.Users[0].UserName))

	// AttachGroupPolicy — idempotent re-attach must not grow the count.
	_, err = iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(grpLifecyclePolicy),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	require.NoError(t, err, "create-policy")

	_, err = iamCli.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(iamPolicyARN(adminAccount, "Ghost")),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	_, err = iamCli.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "attach-group-policy")

	attached, err := iamCli.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "list-attached-group-policies")
	require.Len(t, attached.AttachedPolicies, 1)
	require.Equal(t, policyARN, aws.StringValue(attached.AttachedPolicies[0].PolicyArn))

	_, err = iamCli.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "idempotent re-attach")
	reAttached, err := iamCli.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err)
	require.Len(t, reAttached.AttachedPolicies, 1, "re-attach must be idempotent")

	_, err = iamCli.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{GroupName: aws.String("ghost-group")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// --- Inline group policies (put / get / list) ---

	_, err = iamCli.PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      aws.String("ghost-group"),
		PolicyName:     aws.String(grpLifecycleInline),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// PutGroupPolicy — upsert; re-put with the same name must overwrite, not add.
	_, err = iamCli.PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      aws.String(grpLifecycleGroup),
		PolicyName:     aws.String(grpLifecycleInline),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	require.NoError(t, err, "put-group-policy")

	_, err = iamCli.PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      aws.String(grpLifecycleGroup),
		PolicyName:     aws.String(grpLifecycleInline),
		PolicyDocument: aws.String(groupDescribeRegionsPolicy),
	})
	require.NoError(t, err, "idempotent re-put")

	// GetGroupPolicy — round-trips the stored document (raw, not URL-encoded).
	inlineGot, err := iamCli.GetGroupPolicy(&iam.GetGroupPolicyInput{
		GroupName:  aws.String(grpLifecycleGroup),
		PolicyName: aws.String(grpLifecycleInline),
	})
	require.NoError(t, err, "get-group-policy")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(inlineGot.GroupName))
	require.Equal(t, grpLifecycleInline, aws.StringValue(inlineGot.PolicyName))
	require.JSONEq(t, groupDescribeRegionsPolicy, aws.StringValue(inlineGot.PolicyDocument),
		"get-group-policy must round-trip the stored document")

	_, err = iamCli.GetGroupPolicy(&iam.GetGroupPolicyInput{
		GroupName:  aws.String(grpLifecycleGroup),
		PolicyName: aws.String("ghost-inline"),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// ListGroupPolicies — surfaces exactly the one inline name, never truncated.
	inlineList, err := iamCli.ListGroupPolicies(&iam.ListGroupPoliciesInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "list-group-policies")
	require.Len(t, inlineList.PolicyNames, 1, "exactly one inline policy expected")
	require.Equal(t, grpLifecycleInline, aws.StringValue(inlineList.PolicyNames[0]))
	require.False(t, aws.BoolValue(inlineList.IsTruncated), "list-group-policies is never truncated")

	_, err = iamCli.ListGroupPolicies(&iam.ListGroupPoliciesInput{GroupName: aws.String("ghost-group")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// --- Deletion guards ---

	// delete-user while still a group member → DeleteConflict.
	_, err = iamCli.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(grpLifecycleUser)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	// delete-group while non-empty AND attached → DeleteConflict.
	_, err = iamCli.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	// Remove the member; the attached-policy guard alone must still block delete.
	_, err = iamCli.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "remove-user-from-group")

	_, err = iamCli.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	// detach a policy that isn't attached → NoSuchEntity.
	_, err = iamCli.DetachGroupPolicy(&iam.DetachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(iamPolicyARN(adminAccount, "Ghost")),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// --- Teardown (asserted) ---

	_, err = iamCli.DetachGroupPolicy(&iam.DetachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "detach-group-policy")

	// With the managed policy gone, the inline policy alone must still block
	// the delete — the inline guard, symmetric with the attached-policy guard.
	_, err = iamCli.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	_, err = iamCli.DeleteGroupPolicy(&iam.DeleteGroupPolicyInput{
		GroupName:  aws.String(grpLifecycleGroup),
		PolicyName: aws.String("ghost-inline"),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	_, err = iamCli.DeleteGroupPolicy(&iam.DeleteGroupPolicyInput{
		GroupName:  aws.String(grpLifecycleGroup),
		PolicyName: aws.String(grpLifecycleInline),
	})
	require.NoError(t, err, "delete-group-policy")

	emptyInline, err := iamCli.ListGroupPolicies(&iam.ListGroupPoliciesInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "list-group-policies after delete")
	require.Empty(t, emptyInline.PolicyNames, "inline policy must be gone after delete-group-policy")

	_, err = iamCli.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "delete-group")

	_, err = iamCli.GetGroup(&iam.GetGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// User is no longer a member, so DeleteUser now succeeds.
	_, err = iamCli.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(grpLifecycleUser)})
	require.NoError(t, err, "delete-user")

	_, err = iamCli.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(policyARN)})
	require.NoError(t, err, "delete-policy")
}
