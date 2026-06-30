//go:build e2e

package iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// Dedicated identifiers for the Groups suite. Distinct from the shared
// alice/bob/charlie graph so the group tests can pre-sweep and tear down their
// own state without colliding with the User/Policy phases.
const (
	grpLifecycleGroup  = "grp-e2e-developers"
	grpLifecycleUser   = "grp-e2e-lc-user"
	grpLifecyclePolicy = "grp-e2e-lc-describe-regions"
	grpLifecycleInline = "grp-e2e-lc-inline-describe-regions"

	grpEnforceGroup  = "grp-e2e-enforce"
	grpEnforceUser   = "grp-e2e-enf-user"
	grpEnforcePolicy = "grp-e2e-enf-describe-regions"

	grpInlineEnforceGroup = "grp-e2e-inline-enforce"
	grpInlineEnforceUser  = "grp-e2e-inl-enf-user"
	grpInlineEnforceName  = "grp-e2e-inl-enf-describe-regions"

	// A single-action grant so the enforcement positive case proves a specific
	// group-attached policy was resolved and evaluated — not a blanket allow.
	grpPolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`
)

// runIAMGroupsLifecycle exercises the full group surface: CRUD, membership,
// group-policy attachment, inline group policies (put/get/list), the listing
// reverse-lookups, and every deletion guard (delete-group while attached, while
// inline-policied, while non-empty, delete-user while in a group), then a clean
// teardown. Mirrors runIAMRolesAndProfiles in shape.
func runIAMGroupsLifecycle(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IAM Groups Lifecycle")
	adminAccount := harness.IAMAccountID(t, fix.AWS)
	groupARN := harness.IAMGroupARN(adminAccount, grpLifecycleGroup)
	policyARN := harness.IAMPolicyARN(adminAccount, grpLifecyclePolicy)

	// Defensive sweep — a prior failed run may have left fragments. Order: remove
	// member + detach policy (group delete refuses a non-empty/attached group),
	// then the user, then the policy.
	sweep := func() {
		harness.IAMDeleteGroupBestEffort(fix.AWS, grpLifecycleGroup, []string{grpLifecycleUser}, policyARN)
		iamDeleteUserBestEffort(fix, grpLifecycleUser)
		iamDeletePolicyBestEffort(fix, policyARN)
	}
	sweep()
	fix.Harness.RegisterCleanup(sweep)

	// CreateGroup — happy path; assert ARN + AWS group-id prefix.
	harness.Step(t, "create-group %q", grpLifecycleGroup)
	createOut, err := fix.AWS.IAM.CreateGroup(&iam.CreateGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
	})
	require.NoError(t, err, "create-group")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(createOut.Group.GroupName))
	require.Equal(t, groupARN, aws.StringValue(createOut.Group.Arn),
		"group ARN must follow arn:aws:iam::<acct>:group/<name>")
	require.True(t, len(aws.StringValue(createOut.Group.GroupId)) > 4 &&
		aws.StringValue(createOut.Group.GroupId)[:4] == "AGPA",
		"group id must carry the AWS AGPA prefix, got %q", aws.StringValue(createOut.Group.GroupId))
	harness.Detail(t, "group", grpLifecycleGroup, "arn", groupARN)

	// Duplicate.
	harness.Step(t, "create-group duplicate (expect EntityAlreadyExists)")
	harness.ExpectError(t, "EntityAlreadyExists", func() error {
		_, e := fix.AWS.IAM.CreateGroup(&iam.CreateGroupInput{GroupName: aws.String(grpLifecycleGroup)})
		return e
	})

	// GetGroup — fresh group has an empty (non-nil) member list.
	harness.Step(t, "get-group %q (no members yet)", grpLifecycleGroup)
	got, err := fix.AWS.IAM.GetGroup(&iam.GetGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "get-group")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(got.Group.GroupName))
	require.Empty(t, got.Users, "new group must have zero members")

	harness.Step(t, "get-group nonexistent (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.GetGroup(&iam.GetGroupInput{GroupName: aws.String("ghost-group")})
		return e
	})

	// ListGroups + PathPrefix scan.
	harness.Step(t, "list-groups (>= 1)")
	listed, err := fix.AWS.IAM.ListGroups(&iam.ListGroupsInput{})
	require.NoError(t, err, "list-groups")
	require.GreaterOrEqual(t, len(listed.Groups), 1, "expected >= 1 group, got %d", len(listed.Groups))

	harness.Step(t, "list-groups --path-prefix / (surfaces groups at /)")
	pp, err := fix.AWS.IAM.ListGroups(&iam.ListGroupsInput{PathPrefix: aws.String("/")})
	require.NoError(t, err, "list-groups --path-prefix /")
	require.GreaterOrEqual(t, len(pp.Groups), 1, "path-prefix / must surface groups at /")

	harness.Step(t, "list-groups --path-prefix /nonexistent/ (expect 0)")
	none, err := fix.AWS.IAM.ListGroups(&iam.ListGroupsInput{PathPrefix: aws.String("/nonexistent/")})
	require.NoError(t, err, "list-groups --path-prefix /nonexistent/")
	require.Empty(t, none.Groups, "no group lives under /nonexistent/")

	// Member for the membership + guard sub-steps.
	harness.Step(t, "create-user %q (group member)", grpLifecycleUser)
	_, err = fix.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(grpLifecycleUser)})
	require.NoError(t, err, "create-user")

	// remove-user-from-group on a non-member surfaces NoSuchEntity (must not no-op).
	harness.Step(t, "remove-user-from-group non-member (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
			GroupName: aws.String(grpLifecycleGroup),
			UserName:  aws.String(grpLifecycleUser),
		})
		return e
	})

	// Ghost-entity binds → NoSuchEntity. Group is validated before the user.
	harness.Step(t, "add-user-to-group ghost group (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.AddUserToGroup(&iam.AddUserToGroupInput{
			GroupName: aws.String("ghost-group"),
			UserName:  aws.String(grpLifecycleUser),
		})
		return e
	})
	harness.Step(t, "add-user-to-group ghost user (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.AddUserToGroup(&iam.AddUserToGroupInput{
			GroupName: aws.String(grpLifecycleGroup),
			UserName:  aws.String("ghost-user"),
		})
		return e
	})

	// AddUserToGroup — idempotent re-add must not duplicate the membership.
	harness.Step(t, "add-user-to-group %s <- %s", grpLifecycleGroup, grpLifecycleUser)
	_, err = fix.AWS.IAM.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "add-user-to-group")

	_, err = fix.AWS.IAM.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "idempotent re-add")

	// ListGroupsForUser — reverse lookup sees exactly the one group.
	harness.Step(t, "list-groups-for-user %s (expect 1)", grpLifecycleUser)
	forUser, err := fix.AWS.IAM.ListGroupsForUser(&iam.ListGroupsForUserInput{
		UserName: aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "list-groups-for-user")
	require.Len(t, forUser.Groups, 1, "user should belong to exactly one group")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(forUser.Groups[0].GroupName))

	// GetGroup — member now visible from the group side.
	harness.Step(t, "get-group %q (member visible)", grpLifecycleGroup)
	got, err = fix.AWS.IAM.GetGroup(&iam.GetGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "get-group after add")
	require.Len(t, got.Users, 1, "group should list exactly one member")
	require.Equal(t, grpLifecycleUser, aws.StringValue(got.Users[0].UserName))

	// AttachGroupPolicy — idempotent re-attach must not grow the count.
	harness.Step(t, "create-policy %s (grants ec2:DescribeRegions)", grpLifecyclePolicy)
	_, err = fix.AWS.IAM.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(grpLifecyclePolicy),
		PolicyDocument: aws.String(grpPolicyDoc),
	})
	require.NoError(t, err, "create-policy")

	harness.Step(t, "attach-group-policy unknown policy (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
			GroupName: aws.String(grpLifecycleGroup),
			PolicyArn: aws.String(harness.IAMPolicyARN(adminAccount, "Ghost")),
		})
		return e
	})

	harness.Step(t, "attach-group-policy %s <- %s", grpLifecycleGroup, grpLifecyclePolicy)
	_, err = fix.AWS.IAM.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "attach-group-policy")

	attached, err := fix.AWS.IAM.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String(grpLifecycleGroup),
	})
	require.NoError(t, err, "list-attached-group-policies")
	require.Len(t, attached.AttachedPolicies, 1)
	require.Equal(t, policyARN, aws.StringValue(attached.AttachedPolicies[0].PolicyArn))

	_, err = fix.AWS.IAM.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "idempotent re-attach")
	reAttached, err := fix.AWS.IAM.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String(grpLifecycleGroup),
	})
	require.NoError(t, err)
	require.Len(t, reAttached.AttachedPolicies, 1, "re-attach must be idempotent")

	harness.Step(t, "list-attached-group-policies ghost group (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.ListAttachedGroupPolicies(&iam.ListAttachedGroupPoliciesInput{
			GroupName: aws.String("ghost-group"),
		})
		return e
	})

	// --- Inline group policies (put / get / list) ---

	harness.Step(t, "put-group-policy ghost group (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.PutGroupPolicy(&iam.PutGroupPolicyInput{
			GroupName:      aws.String("ghost-group"),
			PolicyName:     aws.String(grpLifecycleInline),
			PolicyDocument: aws.String(grpPolicyDoc),
		})
		return e
	})

	// PutGroupPolicy — upsert; re-put with the same name must overwrite, not add.
	harness.Step(t, "put-group-policy %s/%s (grants ec2:DescribeRegions)", grpLifecycleGroup, grpLifecycleInline)
	_, err = fix.AWS.IAM.PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      aws.String(grpLifecycleGroup),
		PolicyName:     aws.String(grpLifecycleInline),
		PolicyDocument: aws.String(grpPolicyDoc),
	})
	require.NoError(t, err, "put-group-policy")

	_, err = fix.AWS.IAM.PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      aws.String(grpLifecycleGroup),
		PolicyName:     aws.String(grpLifecycleInline),
		PolicyDocument: aws.String(grpPolicyDoc),
	})
	require.NoError(t, err, "idempotent re-put")

	// GetGroupPolicy — round-trips the stored document (raw, not URL-encoded).
	harness.Step(t, "get-group-policy %s/%s (round-trip)", grpLifecycleGroup, grpLifecycleInline)
	inlineGot, err := fix.AWS.IAM.GetGroupPolicy(&iam.GetGroupPolicyInput{
		GroupName:  aws.String(grpLifecycleGroup),
		PolicyName: aws.String(grpLifecycleInline),
	})
	require.NoError(t, err, "get-group-policy")
	require.Equal(t, grpLifecycleGroup, aws.StringValue(inlineGot.GroupName))
	require.Equal(t, grpLifecycleInline, aws.StringValue(inlineGot.PolicyName))
	require.JSONEq(t, grpPolicyDoc, aws.StringValue(inlineGot.PolicyDocument),
		"get-group-policy must round-trip the stored document")

	harness.Step(t, "get-group-policy unknown name (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.GetGroupPolicy(&iam.GetGroupPolicyInput{
			GroupName:  aws.String(grpLifecycleGroup),
			PolicyName: aws.String("ghost-inline"),
		})
		return e
	})

	// ListGroupPolicies — surfaces exactly the one inline name, never truncated.
	harness.Step(t, "list-group-policies %s (expect 1)", grpLifecycleGroup)
	inlineList, err := fix.AWS.IAM.ListGroupPolicies(&iam.ListGroupPoliciesInput{
		GroupName: aws.String(grpLifecycleGroup),
	})
	require.NoError(t, err, "list-group-policies")
	require.Len(t, inlineList.PolicyNames, 1, "exactly one inline policy expected")
	require.Equal(t, grpLifecycleInline, aws.StringValue(inlineList.PolicyNames[0]))
	require.False(t, aws.BoolValue(inlineList.IsTruncated), "list-group-policies is never truncated")

	harness.Step(t, "list-group-policies ghost group (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.ListGroupPolicies(&iam.ListGroupPoliciesInput{
			GroupName: aws.String("ghost-group"),
		})
		return e
	})

	// --- Deletion guards ---

	// delete-user while still a group member → DeleteConflict.
	harness.Step(t, "delete-user while in group (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(grpLifecycleUser)})
		return e
	})

	// delete-group while non-empty AND attached → DeleteConflict.
	harness.Step(t, "delete-group while member + policy present (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
		return e
	})

	// Remove the member; the attached-policy guard alone must still block delete.
	harness.Step(t, "remove-user-from-group %s", grpLifecycleUser)
	_, err = fix.AWS.IAM.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
		GroupName: aws.String(grpLifecycleGroup),
		UserName:  aws.String(grpLifecycleUser),
	})
	require.NoError(t, err, "remove-user-from-group")

	harness.Step(t, "delete-group while only policy attached (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
		return e
	})

	// detach a policy that isn't attached → NoSuchEntity.
	harness.Step(t, "detach-group-policy not-attached (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.DetachGroupPolicy(&iam.DetachGroupPolicyInput{
			GroupName: aws.String(grpLifecycleGroup),
			PolicyArn: aws.String(harness.IAMPolicyARN(adminAccount, "Ghost")),
		})
		return e
	})

	// --- Teardown (asserted) ---

	harness.Step(t, "detach-group-policy %s", grpLifecyclePolicy)
	_, err = fix.AWS.IAM.DetachGroupPolicy(&iam.DetachGroupPolicyInput{
		GroupName: aws.String(grpLifecycleGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "detach-group-policy")

	// With the managed policy gone, the inline policy alone must still block the
	// delete — the new inline guard, symmetric with the attached-policy guard.
	harness.Step(t, "delete-group while only inline policy present (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
		return e
	})

	harness.Step(t, "delete-group-policy unknown name (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.DeleteGroupPolicy(&iam.DeleteGroupPolicyInput{
			GroupName:  aws.String(grpLifecycleGroup),
			PolicyName: aws.String("ghost-inline"),
		})
		return e
	})

	harness.Step(t, "delete-group-policy %s", grpLifecycleInline)
	_, err = fix.AWS.IAM.DeleteGroupPolicy(&iam.DeleteGroupPolicyInput{
		GroupName:  aws.String(grpLifecycleGroup),
		PolicyName: aws.String(grpLifecycleInline),
	})
	require.NoError(t, err, "delete-group-policy")

	harness.Step(t, "list-group-policies after delete (expect 0)")
	emptyInline, err := fix.AWS.IAM.ListGroupPolicies(&iam.ListGroupPoliciesInput{
		GroupName: aws.String(grpLifecycleGroup),
	})
	require.NoError(t, err, "list-group-policies after delete")
	require.Empty(t, emptyInline.PolicyNames, "inline policy must be gone after delete-group-policy")

	harness.Step(t, "delete-group %s", grpLifecycleGroup)
	_, err = fix.AWS.IAM.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(grpLifecycleGroup)})
	require.NoError(t, err, "delete-group")

	harness.Step(t, "get-group after delete (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.GetGroup(&iam.GetGroupInput{GroupName: aws.String(grpLifecycleGroup)})
		return e
	})

	// User is no longer a member, so DeleteUser now succeeds.
	harness.Step(t, "delete-user %s (no longer in a group)", grpLifecycleUser)
	_, err = fix.AWS.IAM.DeleteUser(&iam.DeleteUserInput{UserName: aws.String(grpLifecycleUser)})
	require.NoError(t, err, "delete-user")

	_, err = fix.AWS.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(policyARN)})
	require.NoError(t, err, "delete-policy")
}

// runIAMGroupEnforcement proves the linchpin: a policy attached to a group
// actually grants its permission to every member. A user with its own access
// key is denied a guarded action; once a policy granting that action is attached
// to a group the user joins, the same live credentials are allowed (policies
// resolved per request); after leaving the group the action is denied again.
// Modelled on runAssumedRoleControlPlaneEnforcement.
func runIAMGroupEnforcement(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IAM Group authorization enforcement")
	adminAccount := harness.IAMAccountID(t, fix.AWS)
	policyARN := harness.IAMPolicyARN(adminAccount, grpEnforcePolicy)

	sweep := func() {
		harness.IAMDeleteGroupBestEffort(fix.AWS, grpEnforceGroup, []string{grpEnforceUser}, policyARN)
		iamDeleteUserBestEffort(fix, grpEnforceUser)
		iamDeletePolicyBestEffort(fix, policyARN)
	}
	sweep()
	fix.Harness.RegisterCleanup(sweep)

	// Member with its own static credentials.
	harness.Step(t, "create-user %q + access key", grpEnforceUser)
	_, err := fix.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(grpEnforceUser)})
	require.NoError(t, err, "create-user")
	key, err := fix.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(grpEnforceUser)})
	require.NoError(t, err, "create-access-key")
	memberCli := harness.NewAWSClientWithCreds(t, fix.Env,
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey))

	// No policy anywhere yet: the active key authenticates, then default-denies.
	harness.Step(t, "describe-regions with no grant (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})

	// Group + policy granting ec2:DescribeRegions, then join the user.
	harness.Step(t, "create-group %q", grpEnforceGroup)
	_, err = fix.AWS.IAM.CreateGroup(&iam.CreateGroupInput{GroupName: aws.String(grpEnforceGroup)})
	require.NoError(t, err, "create-group")

	harness.Step(t, "create+attach policy %q to group (grants ec2:DescribeRegions)", grpEnforcePolicy)
	_, err = fix.AWS.IAM.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(grpEnforcePolicy),
		PolicyDocument: aws.String(grpPolicyDoc),
	})
	require.NoError(t, err, "create-policy")
	_, err = fix.AWS.IAM.AttachGroupPolicy(&iam.AttachGroupPolicyInput{
		GroupName: aws.String(grpEnforceGroup),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "attach-group-policy")

	harness.Step(t, "add-user-to-group %s <- %s", grpEnforceGroup, grpEnforceUser)
	_, err = fix.AWS.IAM.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpEnforceGroup),
		UserName:  aws.String(grpEnforceUser),
	})
	require.NoError(t, err, "add-user-to-group")

	// Same live credentials are now allowed — the grant flows through the group.
	harness.Step(t, "describe-regions after group grant (expect success)")
	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the group grants it")

	// Leave the group and the inherited grant must evaporate.
	harness.Step(t, "remove-user-from-group %s", grpEnforceUser)
	_, err = fix.AWS.IAM.RemoveUserFromGroup(&iam.RemoveUserFromGroupInput{
		GroupName: aws.String(grpEnforceGroup),
		UserName:  aws.String(grpEnforceUser),
	})
	require.NoError(t, err, "remove-user-from-group")

	harness.Step(t, "describe-regions after leaving group (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})
}

// runIAMGroupInlineEnforcement proves the inline-policy linchpin: an inline
// policy embedded in a group grants its permission to every member, exactly as a
// managed attachment does. A user with its own access key is denied a guarded
// action; once an inline policy granting that action is put on a group the user
// joins, the same live credentials are allowed (policies resolved per request);
// after the inline policy is deleted — with the user still a member — the action
// is denied again, proving the inline document was the grant source. Mirrors
// runIAMGroupEnforcement with put-group-policy in place of attach-group-policy.
func runIAMGroupInlineEnforcement(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IAM Group inline-policy authorization enforcement")

	// No managed policy here — the inline doc lives in the group record, so the
	// best-effort sweep clears it via ListGroupPolicies/DeleteGroupPolicy.
	sweep := func() {
		harness.IAMDeleteGroupBestEffort(fix.AWS, grpInlineEnforceGroup, []string{grpInlineEnforceUser})
		iamDeleteUserBestEffort(fix, grpInlineEnforceUser)
	}
	sweep()
	fix.Harness.RegisterCleanup(sweep)

	// Member with its own static credentials.
	harness.Step(t, "create-user %q + access key", grpInlineEnforceUser)
	_, err := fix.AWS.IAM.CreateUser(&iam.CreateUserInput{UserName: aws.String(grpInlineEnforceUser)})
	require.NoError(t, err, "create-user")
	key, err := fix.AWS.IAM.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(grpInlineEnforceUser)})
	require.NoError(t, err, "create-access-key")
	memberCli := harness.NewAWSClientWithCreds(t, fix.Env,
		aws.StringValue(key.AccessKey.AccessKeyId),
		aws.StringValue(key.AccessKey.SecretAccessKey))

	// No grant anywhere yet: the active key authenticates, then default-denies.
	harness.Step(t, "describe-regions with no grant (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})

	// Group with an inline policy granting ec2:DescribeRegions, then join the user.
	harness.Step(t, "create-group %q", grpInlineEnforceGroup)
	_, err = fix.AWS.IAM.CreateGroup(&iam.CreateGroupInput{GroupName: aws.String(grpInlineEnforceGroup)})
	require.NoError(t, err, "create-group")

	harness.Step(t, "put-group-policy %q (grants ec2:DescribeRegions)", grpInlineEnforceName)
	_, err = fix.AWS.IAM.PutGroupPolicy(&iam.PutGroupPolicyInput{
		GroupName:      aws.String(grpInlineEnforceGroup),
		PolicyName:     aws.String(grpInlineEnforceName),
		PolicyDocument: aws.String(grpPolicyDoc),
	})
	require.NoError(t, err, "put-group-policy")

	harness.Step(t, "add-user-to-group %s <- %s", grpInlineEnforceGroup, grpInlineEnforceUser)
	_, err = fix.AWS.IAM.AddUserToGroup(&iam.AddUserToGroupInput{
		GroupName: aws.String(grpInlineEnforceGroup),
		UserName:  aws.String(grpInlineEnforceUser),
	})
	require.NoError(t, err, "add-user-to-group")

	// Same live credentials are now allowed — the inline grant flows through the group.
	harness.Step(t, "describe-regions after inline group grant (expect success)")
	_, err = memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the group inline policy grants it")

	// Delete the inline policy while the user is still a member — the grant must
	// evaporate, isolating the inline document as the sole grant source.
	harness.Step(t, "delete-group-policy %s (user stays a member)", grpInlineEnforceName)
	_, err = fix.AWS.IAM.DeleteGroupPolicy(&iam.DeleteGroupPolicyInput{
		GroupName:  aws.String(grpInlineEnforceGroup),
		PolicyName: aws.String(grpInlineEnforceName),
	})
	require.NoError(t, err, "delete-group-policy")

	harness.Step(t, "describe-regions after inline policy removed (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := memberCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})
}
