package handlers_iam

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestGroup(t *testing.T, svc *IAMServiceImpl, name string) *iam.Group {
	t.Helper()
	out, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String(name),
	})
	require.NoError(t, err)
	return out.Group
}

// ============================================================================
// Group CRUD Tests
// ============================================================================

func TestCreateGroup(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String("developers"),
		Path:      aws.String("/eng/"),
	})
	require.NoError(t, err)
	require.NotNil(t, out.Group)
	assert.Equal(t, "developers", *out.Group.GroupName)
	assert.Equal(t, "/eng/", *out.Group.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":group/eng/developers", *out.Group.Arn)
	require.True(t, len(*out.Group.GroupId) > 4)
	assert.Equal(t, "AGPA", (*out.Group.GroupId)[:4])
}

func TestCreateGroup_DefaultPath(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String("default-path"),
	})
	require.NoError(t, err)
	assert.Equal(t, "/", *out.Group.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":group/default-path", *out.Group.Arn)
}

func TestCreateGroup_Duplicate(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "dup-group")

	_, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String("dup-group"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}

func TestCreateGroup_InvalidName(t *testing.T) {
	svc := setupTestIAMService(t)
	longName := strings.Repeat("a", 129)

	_, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String(longName),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateGroup_InvalidPath(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String("badpath-group"),
		Path:      aws.String("no-leading-slash/"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestGetGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "get-group")
	createTestUser(t, svc, "member")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("get-group"),
		UserName:  aws.String("member"),
	})
	require.NoError(t, err)

	out, err := svc.GetGroup(testAccountID, &iam.GetGroupInput{
		GroupName: aws.String("get-group"),
	})
	require.NoError(t, err)
	assert.Equal(t, "get-group", *out.Group.GroupName)
	require.Len(t, out.Users, 1)
	assert.Equal(t, "member", *out.Users[0].UserName)
}

func TestGetGroup_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetGroup(testAccountID, &iam.GetGroupInput{
		GroupName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetGroup_UsersNeverNil(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "empty-group")

	out, err := svc.GetGroup(testAccountID, &iam.GetGroupInput{
		GroupName: aws.String("empty-group"),
	})
	require.NoError(t, err)
	require.NotNil(t, out.Users, "GetGroupOutput.Users must be an empty slice, never nil")
	assert.Len(t, out.Users, 0)
}

func TestListGroups(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "group1")
	createTestGroup(t, svc, "group2")
	createTestGroup(t, svc, "group3")

	out, err := svc.ListGroups(testAccountID, &iam.ListGroupsInput{})
	require.NoError(t, err)
	require.Len(t, out.Groups, 3)

	names := make(map[string]bool)
	for _, g := range out.Groups {
		names[*g.GroupName] = true
	}
	assert.True(t, names["group1"])
	assert.True(t, names["group2"])
	assert.True(t, names["group3"])
}

func TestListGroups_Empty(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.ListGroups(testAccountID, &iam.ListGroupsInput{})
	require.NoError(t, err)
	assert.Len(t, out.Groups, 0)
}

func TestListGroups_PathPrefix(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String("eng-group"),
		Path:      aws.String("/eng/"),
	})
	require.NoError(t, err)

	_, err = svc.CreateGroup(testAccountID, &iam.CreateGroupInput{
		GroupName: aws.String("ops-group"),
		Path:      aws.String("/ops/"),
	})
	require.NoError(t, err)

	out, err := svc.ListGroups(testAccountID, &iam.ListGroupsInput{
		PathPrefix: aws.String("/eng/"),
	})
	require.NoError(t, err)
	require.Len(t, out.Groups, 1)
	assert.Equal(t, "eng-group", *out.Groups[0].GroupName)
}

func TestDeleteGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "del-group")

	_, err := svc.DeleteGroup(testAccountID, &iam.DeleteGroupInput{
		GroupName: aws.String("del-group"),
	})
	require.NoError(t, err)

	_, err = svc.GetGroup(testAccountID, &iam.GetGroupInput{
		GroupName: aws.String("del-group"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteGroup_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeleteGroup(testAccountID, &iam.DeleteGroupInput{
		GroupName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteGroup_WithAttachedPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "attached-group")
	policy := createTestPolicy(t, svc, "GroupPolicy")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("attached-group"),
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroup(testAccountID, &iam.DeleteGroupInput{
		GroupName: aws.String("attached-group"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

func TestDeleteGroup_WithMembers(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "populated-group")
	createTestUser(t, svc, "occupant")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("populated-group"),
		UserName:  aws.String("occupant"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroup(testAccountID, &iam.DeleteGroupInput{
		GroupName: aws.String("populated-group"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

// ============================================================================
// Group Membership Tests
// ============================================================================

func TestAddUserToGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "team")
	createTestUser(t, svc, "alice")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("team"),
		UserName:  aws.String("alice"),
	})
	require.NoError(t, err)

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("alice"),
	})
	require.NoError(t, err)
	require.Len(t, out.Groups, 1)
	assert.Equal(t, "team", *out.Groups[0].GroupName)
}

func TestAddUserToGroup_Idempotent(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "team")
	createTestUser(t, svc, "bob")

	for range 2 {
		_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
			GroupName: aws.String("team"),
			UserName:  aws.String("bob"),
		})
		require.NoError(t, err)
	}

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("bob"),
	})
	require.NoError(t, err)
	assert.Len(t, out.Groups, 1, "duplicate add must not double-count membership")
}

func TestAddUserToGroup_LimitExceeded(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "busy")

	// Fill the user to the maxGroupsPerUser cap, then prove the next add fails.
	for i := range maxGroupsPerUser {
		name := "g" + strings.Repeat("x", i)
		createTestGroup(t, svc, name)
		_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
			GroupName: aws.String(name),
			UserName:  aws.String("busy"),
		})
		require.NoError(t, err)
	}

	createTestGroup(t, svc, "one-too-many")
	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("one-too-many"),
		UserName:  aws.String("busy"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)
}

func TestAddUserToGroup_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "team")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("team"),
		UserName:  aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestAddUserToGroup_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "carol")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("ghost"),
		UserName:  aws.String("carol"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestRemoveUserFromGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "team")
	createTestUser(t, svc, "dave")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("team"),
		UserName:  aws.String("dave"),
	})
	require.NoError(t, err)

	_, err = svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("team"),
		UserName:  aws.String("dave"),
	})
	require.NoError(t, err)

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("dave"),
	})
	require.NoError(t, err)
	assert.Len(t, out.Groups, 0)
}

func TestRemoveUserFromGroup_NotMember(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "team")
	createTestUser(t, svc, "erin")

	_, err := svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("team"),
		UserName:  aws.String("erin"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestRemoveUserFromGroup_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "team")

	_, err := svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("team"),
		UserName:  aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// TestRemoveUserFromGroup_DanglingPointer proves a membership reference to a
// group that was deleted out from under the user is still cleanable — removal
// operates purely on User.Groups and never fetches the group record.
func TestRemoveUserFromGroup_DanglingPointer(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "doomed")
	createTestUser(t, svc, "frank")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("doomed"),
		UserName:  aws.String("frank"),
	})
	require.NoError(t, err)

	// Delete the group record directly, bypassing the non-empty-group guard.
	require.NoError(t, svc.groupsBucket.Delete(testAccountID+".doomed"))

	_, err = svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("doomed"),
		UserName:  aws.String("frank"),
	})
	require.NoError(t, err)

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("frank"),
	})
	require.NoError(t, err)
	assert.Len(t, out.Groups, 0)
}

func TestListGroupsForUser(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "grace")
	for _, name := range []string{"alpha", "bravo"} {
		createTestGroup(t, svc, name)
		_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
			GroupName: aws.String(name),
			UserName:  aws.String("grace"),
		})
		require.NoError(t, err)
	}

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("grace"),
	})
	require.NoError(t, err)
	require.Len(t, out.Groups, 2)
	names := make(map[string]bool)
	for _, g := range out.Groups {
		names[*g.GroupName] = true
	}
	assert.True(t, names["alpha"])
	assert.True(t, names["bravo"])
}

func TestListGroupsForUser_Empty(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "loner")

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("loner"),
	})
	require.NoError(t, err)
	assert.Len(t, out.Groups, 0)
}

func TestListGroupsForUser_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// TestListGroupsForUser_SkipsMissingGroup proves a dangling membership pointer
// is silently skipped rather than erroring the whole listing.
func TestListGroupsForUser_SkipsMissingGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "live")
	createTestGroup(t, svc, "vanishing")
	createTestUser(t, svc, "henry")

	for _, name := range []string{"live", "vanishing"} {
		_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
			GroupName: aws.String(name),
			UserName:  aws.String("henry"),
		})
		require.NoError(t, err)
	}

	require.NoError(t, svc.groupsBucket.Delete(testAccountID+".vanishing"))

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("henry"),
	})
	require.NoError(t, err)
	require.Len(t, out.Groups, 1)
	assert.Equal(t, "live", *out.Groups[0].GroupName)
}

// ============================================================================
// Group Policy Attachment Tests
// ============================================================================

func TestAttachGroupPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "policy-group")
	policy := createTestPolicy(t, svc, "AttachPolicy")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("policy-group"),
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	out, err := svc.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("policy-group"),
	})
	require.NoError(t, err)
	require.Len(t, out.AttachedPolicies, 1)
	assert.Equal(t, *policy.Arn, *out.AttachedPolicies[0].PolicyArn)
	assert.Equal(t, "AttachPolicy", *out.AttachedPolicies[0].PolicyName)
}

func TestAttachGroupPolicy_Idempotent(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "idem-group")
	policy := createTestPolicy(t, svc, "IdemPolicy")

	for range 2 {
		_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
			GroupName: aws.String("idem-group"),
			PolicyArn: policy.Arn,
		})
		require.NoError(t, err)
	}

	out, err := svc.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("idem-group"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 1, "duplicate attach must not double-count")
}

func TestAttachGroupPolicy_PolicyNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "needs-policy")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("needs-policy"),
		PolicyArn: aws.String("arn:aws:iam::" + testAccountID + ":policy/Ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestAttachGroupPolicy_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "OrphanPolicy")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("ghost"),
		PolicyArn: policy.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// TestAttachGroupPolicy_AWSManagedOpaque proves an AWS-managed ARN with no
// backing policy record round-trips opaquely instead of failing NoSuchEntity.
func TestAttachGroupPolicy_AWSManagedOpaque(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "managed-group")
	const managedARN = "arn:aws:iam::aws:policy/service-role/AmazonEKS_CNI_Policy"

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("managed-group"),
		PolicyArn: aws.String(managedARN),
	})
	require.NoError(t, err)

	out, err := svc.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("managed-group"),
	})
	require.NoError(t, err)
	require.Len(t, out.AttachedPolicies, 1)
	assert.Equal(t, managedARN, *out.AttachedPolicies[0].PolicyArn)
	assert.Equal(t, "AmazonEKS_CNI_Policy", *out.AttachedPolicies[0].PolicyName)
}

func TestDetachGroupPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "detach-group")
	policy := createTestPolicy(t, svc, "DetachPolicy")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("detach-group"),
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DetachGroupPolicy(testAccountID, &iam.DetachGroupPolicyInput{
		GroupName: aws.String("detach-group"),
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	out, err := svc.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("detach-group"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 0)
}

func TestDetachGroupPolicy_NotAttached(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "bare-group")
	policy := createTestPolicy(t, svc, "NotAttachedPolicy")

	_, err := svc.DetachGroupPolicy(testAccountID, &iam.DetachGroupPolicyInput{
		GroupName: aws.String("bare-group"),
		PolicyArn: policy.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDetachGroupPolicy_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "OrphanDetach")

	_, err := svc.DetachGroupPolicy(testAccountID, &iam.DetachGroupPolicyInput{
		GroupName: aws.String("ghost"),
		PolicyArn: policy.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListAttachedGroupPolicies_Empty(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "empty-attach")

	out, err := svc.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("empty-attach"),
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 0)
}

func TestListAttachedGroupPolicies_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListAttachedGroupPolicies(testAccountID, &iam.ListAttachedGroupPoliciesInput{
		GroupName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// ============================================================================
// Authorization Integration — the linchpin (GetUserPolicies resolves groups)
// ============================================================================

// TestGetUserPolicies_GroupInheritance is the load-bearing test: a policy
// attached to a group must contribute its grant to every member's effective
// permission set, and stop contributing once the user leaves the group. Without
// this behaviour groups are cosmetic and grant nothing.
func TestGetUserPolicies_GroupInheritance(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "grantor")
	createTestUser(t, svc, "ivy")
	policy := createTestPolicy(t, svc, "GroupGrant")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("grantor"),
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	// Before joining: the group's grant is absent.
	docs, err := svc.GetUserPolicies(testAccountID, "ivy")
	require.NoError(t, err)
	assert.False(t, policiesGrant(docs, "ec2:DescribeInstances"), "grant must be absent before joining")

	_, err = svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("grantor"),
		UserName:  aws.String("ivy"),
	})
	require.NoError(t, err)

	// After joining: the group's grant flows through.
	docs, err = svc.GetUserPolicies(testAccountID, "ivy")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.True(t, policiesGrant(docs, "ec2:DescribeInstances"), "group grant must flow to the member")

	// The grant is group-inherited, not a direct attachment.
	attached, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String("ivy"),
	})
	require.NoError(t, err)
	assert.Empty(t, attached.AttachedPolicies, "group policy must not appear as a directly attached user policy")

	// After leaving: the grant disappears again.
	_, err = svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("grantor"),
		UserName:  aws.String("ivy"),
	})
	require.NoError(t, err)

	docs, err = svc.GetUserPolicies(testAccountID, "ivy")
	require.NoError(t, err)
	assert.False(t, policiesGrant(docs, "ec2:DescribeInstances"), "grant must vanish after leaving the group")
}

// TestGetUserPolicies_DirectAndGroup proves direct user attachments and
// group-inherited policies both surface in one combined effective set.
func TestGetUserPolicies_DirectAndGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "combined-group")
	createTestUser(t, svc, "jack")
	direct := createTestPolicy(t, svc, "DirectPolicy")
	grouped := createTestPolicy(t, svc, "GroupedPolicy")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("jack"),
		PolicyArn: direct.Arn,
	})
	require.NoError(t, err)

	_, err = svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("combined-group"),
		PolicyArn: grouped.Arn,
	})
	require.NoError(t, err)

	_, err = svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("combined-group"),
		UserName:  aws.String("jack"),
	})
	require.NoError(t, err)

	docs, err := svc.GetUserPolicies(testAccountID, "jack")
	require.NoError(t, err)
	assert.Len(t, docs, 2, "both the direct and the group-inherited policy must resolve")
}

// TestGetUserPolicies_SkipsMissingGroup proves a dangling membership pointer is
// skipped (not fail-closed): the user keeps their direct grants and the dangling
// name remains cleanable via RemoveUserFromGroup.
func TestGetUserPolicies_SkipsMissingGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "soon-gone")
	createTestUser(t, svc, "kim")
	direct := createTestPolicy(t, svc, "KimDirect")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("kim"),
		PolicyArn: direct.Arn,
	})
	require.NoError(t, err)

	_, err = svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("soon-gone"),
		UserName:  aws.String("kim"),
	})
	require.NoError(t, err)

	// Delete the group record directly, leaving a dangling User.Groups pointer.
	require.NoError(t, svc.groupsBucket.Delete(testAccountID+".soon-gone"))

	docs, err := svc.GetUserPolicies(testAccountID, "kim")
	require.NoError(t, err, "a missing group must be skipped, not fail closed")
	require.Len(t, docs, 1, "the user's direct policy must still resolve")
	assert.True(t, policiesGrant(docs, "ec2:DescribeInstances"))

	// The dangling name is still cleanable.
	_, err = svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("soon-gone"),
		UserName:  aws.String("kim"),
	})
	require.NoError(t, err)
}

// ============================================================================
// DeleteUser group guard
// ============================================================================

func TestDeleteUser_InGroup(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "blocker")
	createTestUser(t, svc, "leo")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("blocker"),
		UserName:  aws.String("leo"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("leo"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)

	// Succeeds once the user leaves the group.
	_, err = svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("blocker"),
		UserName:  aws.String("leo"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("leo"),
	})
	require.NoError(t, err)
}

// ============================================================================
// Account Scoping
// ============================================================================

func TestGroups_AccountScoping(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Org B")
	require.NoError(t, err)

	// Same group name in two accounts must not collide.
	_, err = svc.CreateGroup(accA.AccountID, &iam.CreateGroupInput{GroupName: aws.String("shared-name")})
	require.NoError(t, err)
	_, err = svc.CreateGroup(accB.AccountID, &iam.CreateGroupInput{GroupName: aws.String("shared-name")})
	require.NoError(t, err)

	listA, err := svc.ListGroups(accA.AccountID, &iam.ListGroupsInput{})
	require.NoError(t, err)
	require.Len(t, listA.Groups, 1)
	assert.Contains(t, *listA.Groups[0].Arn, accA.AccountID)

	listB, err := svc.ListGroups(accB.AccountID, &iam.ListGroupsInput{})
	require.NoError(t, err)
	require.Len(t, listB.Groups, 1)
	assert.Contains(t, *listB.Groups[0].Arn, accB.AccountID)

	// Deleting in A leaves B's identically-named group intact.
	_, err = svc.DeleteGroup(accA.AccountID, &iam.DeleteGroupInput{GroupName: aws.String("shared-name")})
	require.NoError(t, err)

	_, err = svc.GetGroup(accB.AccountID, &iam.GetGroupInput{GroupName: aws.String("shared-name")})
	require.NoError(t, err, "Account B's group must survive Account A's delete")
}
