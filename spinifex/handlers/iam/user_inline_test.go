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

// ============================================================================
// User Inline Policy Tests (Put / Get / Delete / List)
// ============================================================================

func TestPutUserPolicy_RoundTrip(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "inline-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("inline-user"),
		PolicyName:     aws.String("AllowDescribe"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	out, err := svc.GetUserPolicy(testAccountID, &iam.GetUserPolicyInput{
		UserName:   aws.String("inline-user"),
		PolicyName: aws.String("AllowDescribe"),
	})
	require.NoError(t, err)
	assert.Equal(t, "inline-user", *out.UserName)
	assert.Equal(t, "AllowDescribe", *out.PolicyName)
	assert.Equal(t, validPolicyDocument(), *out.PolicyDocument)
}

func TestPutUserPolicy_IdempotentOverwrite(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "overwrite-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("overwrite-user"),
		PolicyName:     aws.String("Policy"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("overwrite-user"),
		PolicyName:     aws.String("Policy"),
		PolicyDocument: aws.String(inlineDenyDocument()),
	})
	require.NoError(t, err)

	out, err := svc.GetUserPolicy(testAccountID, &iam.GetUserPolicyInput{
		UserName:   aws.String("overwrite-user"),
		PolicyName: aws.String("Policy"),
	})
	require.NoError(t, err)
	assert.Equal(t, inlineDenyDocument(), *out.PolicyDocument)

	list, err := svc.ListUserPolicies(testAccountID, &iam.ListUserPoliciesInput{
		UserName: aws.String("overwrite-user"),
	})
	require.NoError(t, err)
	assert.Len(t, list.PolicyNames, 1, "overwrite must not duplicate the name")
}

func TestPutUserPolicy_InvalidName(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "badname-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("badname-user"),
		PolicyName:     aws.String("bad name"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestPutUserPolicy_MalformedDocument(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "malformed-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("malformed-user"),
		PolicyName:     aws.String("Bad"),
		PolicyDocument: aws.String(`{not valid json`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestPutUserPolicy_OversizedDocument(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "oversized-user")

	huge := strings.Repeat("a", maxPolicyDocumentSize+1)
	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("oversized-user"),
		PolicyName:     aws.String("Huge"),
		PolicyDocument: aws.String(huge),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestPutUserPolicy_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("ghost"),
		PolicyName:     aws.String("Policy"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetUserPolicy_UnknownName(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "get-unknown")

	_, err := svc.GetUserPolicy(testAccountID, &iam.GetUserPolicyInput{
		UserName:   aws.String("get-unknown"),
		PolicyName: aws.String("missing"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetUserPolicy_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetUserPolicy(testAccountID, &iam.GetUserPolicyInput{
		UserName:   aws.String("ghost"),
		PolicyName: aws.String("Policy"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListUserPolicies_Sorted(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "list-user")

	for _, name := range []string{"Charlie", "Alpha", "Bravo"} {
		_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
			UserName:       aws.String("list-user"),
			PolicyName:     aws.String(name),
			PolicyDocument: aws.String(validPolicyDocument()),
		})
		require.NoError(t, err)
	}

	out, err := svc.ListUserPolicies(testAccountID, &iam.ListUserPoliciesInput{
		UserName: aws.String("list-user"),
	})
	require.NoError(t, err)
	require.False(t, *out.IsTruncated)
	got := make([]string, 0, len(out.PolicyNames))
	for _, n := range out.PolicyNames {
		got = append(got, *n)
	}
	assert.Equal(t, []string{"Alpha", "Bravo", "Charlie"}, got)
}

func TestListUserPolicies_Empty(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "empty-inline-user")

	out, err := svc.ListUserPolicies(testAccountID, &iam.ListUserPoliciesInput{
		UserName: aws.String("empty-inline-user"),
	})
	require.NoError(t, err)
	assert.NotNil(t, out.PolicyNames)
	assert.Empty(t, out.PolicyNames)
	assert.False(t, *out.IsTruncated)
}

func TestListUserPolicies_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListUserPolicies(testAccountID, &iam.ListUserPoliciesInput{
		UserName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteUserPolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "del-inline-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("del-inline-user"),
		PolicyName:     aws.String("Doomed"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
		UserName:   aws.String("del-inline-user"),
		PolicyName: aws.String("Doomed"),
	})
	require.NoError(t, err)

	_, err = svc.GetUserPolicy(testAccountID, &iam.GetUserPolicyInput{
		UserName:   aws.String("del-inline-user"),
		PolicyName: aws.String("Doomed"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)

	list, err := svc.ListUserPolicies(testAccountID, &iam.ListUserPoliciesInput{
		UserName: aws.String("del-inline-user"),
	})
	require.NoError(t, err)
	assert.Empty(t, list.PolicyNames)
}

func TestDeleteUserPolicy_DoubleDelete(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "double-del-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("double-del-user"),
		PolicyName:     aws.String("Once"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
		UserName:   aws.String("double-del-user"),
		PolicyName: aws.String("Once"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
		UserName:   aws.String("double-del-user"),
		PolicyName: aws.String("Once"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteUserPolicy_UserNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
		UserName:   aws.String("ghost"),
		PolicyName: aws.String("Policy"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// ============================================================================
// Authorization Integration — the linchpin (GetUserPolicies resolves user inline)
// ============================================================================

// putRawUserInlinePolicy writes an inline policy document into a user record
// directly via the bucket, bypassing PutUserPolicy validation. Used to plant a
// corrupt stored document that the write path would otherwise reject.
func putRawUserInlinePolicy(t *testing.T, svc *IAMServiceImpl, userName, policyName, raw string) {
	t.Helper()
	user, err := svc.getUser(t.Context(), testAccountID, userName)
	require.NoError(t, err)
	if user.InlinePolicies == nil {
		user.InlinePolicies = map[string]string{}
	}
	user.InlinePolicies[policyName] = raw
	data, err := json.Marshal(user)
	require.NoError(t, err)
	_, err = svc.usersBucket.Put(t.Context(), testAccountID+"."+userName, data)
	require.NoError(t, err)
}

// TestGetUserPolicies_UserInlineEnforced is the linchpin: a user's own inline
// policy must contribute its grant to the user's effective permission set, and
// stop contributing once the inline policy is removed. Without this, put-user-policy
// appears to succeed while granting nothing — a silent no-op.
func TestGetUserPolicies_UserInlineEnforced(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "quinn")

	// Before the inline policy exists: the grant is absent.
	docs, err := svc.GetUserPolicies(testAccountID, "quinn")
	require.NoError(t, err)
	assert.False(t, policiesGrant(docs, "ec2:DescribeInstances"), "grant must be absent before the inline policy")

	_, err = svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("quinn"),
		PolicyName:     aws.String("InlineGrant"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	// After the inline policy is added: the grant flows through.
	docs, err = svc.GetUserPolicies(testAccountID, "quinn")
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.True(t, policiesGrant(docs, "ec2:DescribeInstances"), "user inline grant must be enforced")

	// After removing the inline policy: the grant disappears again.
	_, err = svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
		UserName:   aws.String("quinn"),
		PolicyName: aws.String("InlineGrant"),
	})
	require.NoError(t, err)

	docs, err = svc.GetUserPolicies(testAccountID, "quinn")
	require.NoError(t, err)
	assert.False(t, policiesGrant(docs, "ec2:DescribeInstances"), "grant must vanish after the inline policy is deleted")
}

// TestGetUserPolicies_UserManagedAndInline proves a user's direct managed
// attachment and its own inline document both surface in one combined effective
// set — inline grants supplement managed grants rather than replacing them, so a
// deny-wins evaluator can honour an inline Deny alongside a managed Allow.
func TestGetUserPolicies_UserManagedAndInline(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "rita")
	managed := createTestPolicy(t, svc, "ManagedGrant")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{
		UserName:  aws.String("rita"),
		PolicyArn: managed.Arn,
	})
	require.NoError(t, err)

	_, err = svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("rita"),
		PolicyName:     aws.String("InlineDeny"),
		PolicyDocument: aws.String(inlineDenyDocument()),
	})
	require.NoError(t, err)

	docs, err := svc.GetUserPolicies(testAccountID, "rita")
	require.NoError(t, err)
	require.Len(t, docs, 2, "both the managed attachment and the inline document must resolve")
	assert.True(t, policiesGrant(docs, "ec2:DescribeInstances"), "managed Allow surfaced")
	assert.True(t, policiesGrant(docs, "s3:DeleteObject"), "inline doc surfaced")
}

// TestGetUserPolicies_UserInlineMalformedFailsClosed proves a corrupt inline
// document on a user fails the whole resolution closed rather than silently
// dropping the grant source, mirroring the group-inline handling.
func TestGetUserPolicies_UserInlineMalformedFailsClosed(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "sam")

	putRawUserInlinePolicy(t, svc, "sam", "Bad", `{not valid json`)

	_, err := svc.GetUserPolicies(testAccountID, "sam")
	assert.Error(t, err, "a malformed user inline doc must fail closed")
}

// ============================================================================
// DeleteUser inline-policy guard
// ============================================================================

// TestDeleteUser_WithInlinePolicy proves a user carrying an inline policy refuses
// deletion until the inline policy is removed, mirroring DeleteGroup.
func TestDeleteUser_WithInlinePolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "inline-conflict-user")

	_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName:       aws.String("inline-conflict-user"),
		PolicyName:     aws.String("Blocker"),
		PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("inline-conflict-user"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)

	// Succeeds once the inline policy is removed.
	_, err = svc.DeleteUserPolicy(testAccountID, &iam.DeleteUserPolicyInput{
		UserName:   aws.String("inline-conflict-user"),
		PolicyName: aws.String("Blocker"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteUser(testAccountID, &iam.DeleteUserInput{
		UserName: aws.String("inline-conflict-user"),
	})
	require.NoError(t, err)
}
