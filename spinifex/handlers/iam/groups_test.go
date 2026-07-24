package handlers_iam

import (
	"encoding/json"
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
	require.Greater(t, len(*out.Group.GroupId), 4)
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
	assert.Empty(t, out.Users)
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
	assert.Empty(t, out.Groups)
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
	assert.Empty(t, out.Groups)
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
	require.NoError(t, svc.groupsBucket.Delete(t.Context(), testAccountID+".doomed"))

	_, err = svc.RemoveUserFromGroup(testAccountID, &iam.RemoveUserFromGroupInput{
		GroupName: aws.String("doomed"),
		UserName:  aws.String("frank"),
	})
	require.NoError(t, err)

	out, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{
		UserName: aws.String("frank"),
	})
	require.NoError(t, err)
	assert.Empty(t, out.Groups)
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
	assert.Empty(t, out.Groups)
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

	require.NoError(t, svc.groupsBucket.Delete(t.Context(), testAccountID+".vanishing"))

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
	assert.Empty(t, out.AttachedPolicies)
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
	assert.Empty(t, out.AttachedPolicies)
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
// Group Inline Policy Tests (Put / Get / Delete / List)
// ============================================================================

func TestPutGroupPolicy_RoundTrip(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "inline-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("inline-group"),
		PolicyName:     aws.String("AllowDescribe"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	out, err := svc.GetGroupPolicy(testAccountID, &iam.GetGroupPolicyInput{
		GroupName:  aws.String("inline-group"),
		PolicyName: aws.String("AllowDescribe"),
	})
	require.NoError(t, err)
	assert.Equal(t, "inline-group", *out.GroupName)
	assert.Equal(t, "AllowDescribe", *out.PolicyName)
	assert.Equal(t, validPolicyDocument(), *out.PolicyDocument)
}

func TestPutGroupPolicy_IdempotentOverwrite(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "overwrite-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("overwrite-group"),
		PolicyName:     aws.String("Policy"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("overwrite-group"),
		PolicyName:     aws.String("Policy"),
		PolicyDocument: aws.String(inlineDenyDocument()),
	})
	require.NoError(t, err)

	out, err := svc.GetGroupPolicy(testAccountID, &iam.GetGroupPolicyInput{
		GroupName:  aws.String("overwrite-group"),
		PolicyName: aws.String("Policy"),
	})
	require.NoError(t, err)
	assert.Equal(t, inlineDenyDocument(), *out.PolicyDocument)

	list, err := svc.ListGroupPolicies(testAccountID, &iam.ListGroupPoliciesInput{
		GroupName: aws.String("overwrite-group"),
	})
	require.NoError(t, err)
	assert.Len(t, list.PolicyNames, 1, "overwrite must not duplicate the name")
}

func TestPutGroupPolicy_InvalidName(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "badname-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("badname-group"),
		PolicyName:     aws.String("bad name"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestPutGroupPolicy_MalformedDocument(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "malformed-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("malformed-group"),
		PolicyName:     aws.String("Bad"),
		PolicyDocument: aws.String(`{not valid json`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestPutGroupPolicy_OversizedDocument(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "oversized-group")

	huge := strings.Repeat("a", maxPolicyDocumentSize+1)
	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("oversized-group"),
		PolicyName:     aws.String("Huge"),
		PolicyDocument: aws.String(huge),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestPutGroupPolicy_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("ghost"),
		PolicyName:     aws.String("Policy"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetGroupPolicy_UnknownName(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "get-unknown")

	_, err := svc.GetGroupPolicy(testAccountID, &iam.GetGroupPolicyInput{
		GroupName:  aws.String("get-unknown"),
		PolicyName: aws.String("missing"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetGroupPolicy_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetGroupPolicy(testAccountID, &iam.GetGroupPolicyInput{
		GroupName:  aws.String("ghost"),
		PolicyName: aws.String("Policy"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListGroupPolicies_Sorted(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "list-group")

	for _, name := range []string{"Charlie", "Alpha", "Bravo"} {
		_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
			GroupName:      aws.String("list-group"),
			PolicyName:     aws.String(name),
			PolicyDocument: aws.String(validPolicyDocument()),
		})
		require.NoError(t, err)
	}

	out, err := svc.ListGroupPolicies(testAccountID, &iam.ListGroupPoliciesInput{
		GroupName: aws.String("list-group"),
	})
	require.NoError(t, err)
	require.False(t, *out.IsTruncated)
	got := make([]string, 0, len(out.PolicyNames))
	for _, n := range out.PolicyNames {
		got = append(got, *n)
	}
	assert.Equal(t, []string{"Alpha", "Bravo", "Charlie"}, got)
}

func TestListGroupPolicies_Empty(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "empty-inline-group")

	out, err := svc.ListGroupPolicies(testAccountID, &iam.ListGroupPoliciesInput{
		GroupName: aws.String("empty-inline-group"),
	})
	require.NoError(t, err)
	assert.NotNil(t, out.PolicyNames)
	assert.Empty(t, out.PolicyNames)
	assert.False(t, *out.IsTruncated)
}

func TestListGroupPolicies_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListGroupPolicies(testAccountID, &iam.ListGroupPoliciesInput{
		GroupName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteGroupPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "del-inline-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("del-inline-group"),
		PolicyName:     aws.String("Doomed"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
		GroupName:  aws.String("del-inline-group"),
		PolicyName: aws.String("Doomed"),
	})
	require.NoError(t, err)

	_, err = svc.GetGroupPolicy(testAccountID, &iam.GetGroupPolicyInput{
		GroupName:  aws.String("del-inline-group"),
		PolicyName: aws.String("Doomed"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)

	list, err := svc.ListGroupPolicies(testAccountID, &iam.ListGroupPoliciesInput{
		GroupName: aws.String("del-inline-group"),
	})
	require.NoError(t, err)
	assert.Empty(t, list.PolicyNames)
}

func TestDeleteGroupPolicy_DoubleDelete(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "double-del-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("double-del-group"),
		PolicyName:     aws.String("Once"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
		GroupName:  aws.String("double-del-group"),
		PolicyName: aws.String("Once"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
		GroupName:  aws.String("double-del-group"),
		PolicyName: aws.String("Once"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteGroupPolicy_GroupNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
		GroupName:  aws.String("ghost"),
		PolicyName: aws.String("Policy"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// TestDeleteGroup_WithInlinePolicy proves a group carrying an inline policy
// refuses deletion until the inline policy is removed, mirroring DeleteRole.
func TestDeleteGroup_WithInlinePolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "inline-conflict-group")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("inline-conflict-group"),
		PolicyName:     aws.String("Blocker"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroup(testAccountID, &iam.DeleteGroupInput{
		GroupName: aws.String("inline-conflict-group"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)

	// Succeeds once the inline policy is removed.
	_, err = svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
		GroupName:  aws.String("inline-conflict-group"),
		PolicyName: aws.String("Blocker"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteGroup(testAccountID, &iam.DeleteGroupInput{
		GroupName: aws.String("inline-conflict-group"),
	})
	require.NoError(t, err)
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
	require.NoError(t, svc.groupsBucket.Delete(t.Context(), testAccountID+".soon-gone"))

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

// putRawGroupInlinePolicy writes an inline policy document into a group record
// directly via the bucket, bypassing PutGroupPolicy validation. Used to plant a
// corrupt stored document that the write path would otherwise reject.
func putRawGroupInlinePolicy(t *testing.T, svc *IAMServiceImpl, groupName, policyName, raw string) {
	t.Helper()
	group, err := svc.getGroup(t.Context(), testAccountID, groupName)
	require.NoError(t, err)
	if group.InlinePolicies == nil {
		group.InlinePolicies = map[string]string{}
	}
	group.InlinePolicies[policyName] = raw
	data, err := json.Marshal(group)
	require.NoError(t, err)
	_, err = svc.groupsBucket.Put(t.Context(), testAccountID+"."+groupName, data)
	require.NoError(t, err)
}

// TestGetUserPolicies_GroupInlineInheritance is the linchpin: an inline policy
// embedded in a group must contribute its grant to every member's effective
// permission set, and stop contributing once the inline policy is removed.
// Without this, group inline policies are cosmetic and grant nothing.
func TestGetUserPolicies_GroupInlineInheritance(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "inline-grantor")
	createTestUser(t, svc, "nora")

	_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("inline-grantor"),
		PolicyName:     aws.String("InlineGrant"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	// Before joining: the group's inline grant is absent.
	docs, err := svc.GetUserPolicies(testAccountID, "nora")
	require.NoError(t, err)
	assert.False(t, policiesGrant(docs, "ec2:DescribeInstances"), "grant must be absent before joining")

	_, err = svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("inline-grantor"),
		UserName:  aws.String("nora"),
	})
	require.NoError(t, err)

	// After joining: the inline grant flows through to the member.
	docs, err = svc.GetUserPolicies(testAccountID, "nora")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.True(t, policiesGrant(docs, "ec2:DescribeInstances"), "group inline grant must flow to the member")

	// After removing the inline policy: the grant disappears again.
	_, err = svc.DeleteGroupPolicy(testAccountID, &iam.DeleteGroupPolicyInput{
		GroupName:  aws.String("inline-grantor"),
		PolicyName: aws.String("InlineGrant"),
	})
	require.NoError(t, err)

	docs, err = svc.GetUserPolicies(testAccountID, "nora")
	require.NoError(t, err)
	assert.False(t, policiesGrant(docs, "ec2:DescribeInstances"), "grant must vanish after the inline policy is deleted")
}

// TestGetUserPolicies_GroupManagedAndInline proves a group's managed attachment
// and its inline document both surface in a member's combined effective set —
// inline grants supplement managed grants rather than replacing them.
func TestGetUserPolicies_GroupManagedAndInline(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "combined-inline-group")
	createTestUser(t, svc, "omar")
	managed := createTestPolicy(t, svc, "ManagedGrant")

	_, err := svc.AttachGroupPolicy(testAccountID, &iam.AttachGroupPolicyInput{
		GroupName: aws.String("combined-inline-group"),
		PolicyArn: managed.Arn,
	})
	require.NoError(t, err)

	_, err = svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
		GroupName:      aws.String("combined-inline-group"),
		PolicyName:     aws.String("InlineDeny"),
		PolicyDocument: aws.String(inlineDenyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("combined-inline-group"),
		UserName:  aws.String("omar"),
	})
	require.NoError(t, err)

	docs, err := svc.GetUserPolicies(testAccountID, "omar")
	require.NoError(t, err)
	require.Len(t, docs, 2, "both the managed attachment and the inline document must resolve")
	assert.True(t, policiesGrant(docs, "ec2:DescribeInstances"), "managed Allow surfaced")
	assert.True(t, policiesGrant(docs, "s3:DeleteObject"), "inline doc surfaced")
}

// TestGetUserPolicies_GroupInlineMalformedFailsClosed proves a corrupt inline
// document on a resolvable group fails the whole resolution closed rather than
// silently dropping the grant source, mirroring GetRolePolicies.
func TestGetUserPolicies_GroupInlineMalformedFailsClosed(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestGroup(t, svc, "corrupt-inline-group")
	createTestUser(t, svc, "pam")

	_, err := svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{
		GroupName: aws.String("corrupt-inline-group"),
		UserName:  aws.String("pam"),
	})
	require.NoError(t, err)

	putRawGroupInlinePolicy(t, svc, "corrupt-inline-group", "Bad", `{not valid json`)

	_, err = svc.GetUserPolicies(testAccountID, "pam")
	assert.Error(t, err, "a malformed inline doc on a resolvable group must fail closed")
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
