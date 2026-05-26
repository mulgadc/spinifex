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

// validTrustPolicy returns a minimal valid trust policy JSON document
// that allows ec2.amazonaws.com to assume the role.
func validTrustPolicy() string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
}

func createTestRole(t *testing.T, svc *IAMServiceImpl, name string) *iam.Role {
	t.Helper()
	out, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String(name),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	require.NoError(t, err)
	return out.Role
}

// ============================================================================
// Role CRUD Tests
// ============================================================================

func TestCreateRole(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("app-role"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		Path:                     aws.String("/service-roles/"),
		Description:              aws.String("Role for app servers"),
		MaxSessionDuration:       aws.Int64(7200),
		Tags: []*iam.Tag{
			{Key: aws.String("team"), Value: aws.String("backend")},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, out.Role)
	assert.Equal(t, "app-role", *out.Role.RoleName)
	assert.Equal(t, "/service-roles/", *out.Role.Path)
	assert.Equal(t, "Role for app servers", *out.Role.Description)
	assert.Equal(t, int64(7200), *out.Role.MaxSessionDuration)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":role/service-roles/app-role", *out.Role.Arn)
	require.True(t, len(*out.Role.RoleId) > 4)
	assert.Equal(t, "AROA", (*out.Role.RoleId)[:4])
	require.Len(t, out.Role.Tags, 1)
	assert.Equal(t, "team", *out.Role.Tags[0].Key)
	assert.Equal(t, "backend", *out.Role.Tags[0].Value)
}

func TestCreateRole_DefaultPath(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("default-path"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	require.NoError(t, err)
	assert.Equal(t, "/", *out.Role.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":role/default-path", *out.Role.Arn)
}

func TestCreateRole_DefaultMaxSessionDuration(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("default-session"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	require.NoError(t, err)
	assert.Equal(t, defaultMaxSessionDuration, *out.Role.MaxSessionDuration)
}

func TestCreateRole_InvalidName(t *testing.T) {
	svc := setupTestIAMService(t)
	longName := strings.Repeat("a", 65)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String(longName),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateRole_InvalidPath(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("badpath-role"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		Path:                     aws.String("no-leading-slash/"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateRole_MalformedTrustPolicy_InvalidJSON(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("badtrust"),
		AssumeRolePolicyDocument: aws.String(`{not valid json`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreateRole_MalformedTrustPolicy_WrongVersion(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("wrongversion"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreateRole_MalformedTrustPolicy_NoStatements(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("nostmts"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreateRole_MalformedTrustPolicy_MissingPrincipal(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("noprincipal"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sts:AssumeRole"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreateRole_MalformedTrustPolicy_BadEffect(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("bad-effect"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreateRole_MalformedTrustPolicy_TooLarge(t *testing.T) {
	svc := setupTestIAMService(t)

	// Pad the Sid to push the doc past 2048 bytes.
	pad := strings.Repeat("a", maxTrustPolicyDocumentSize)
	doc := `{"Version":"2012-10-17","Statement":[{"Sid":"` + pad + `","Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("toolarge"),
		AssumeRolePolicyDocument: aws.String(doc),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreateRole_TrustPolicy_DenyEffectAccepted(t *testing.T) {
	svc := setupTestIAMService(t)

	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"sts:AssumeRole"}]}`
	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("deny-role"),
		AssumeRolePolicyDocument: aws.String(doc),
	})
	require.NoError(t, err)
}

func TestCreateRole_MaxSessionDuration_TooSmall(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("short-session"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		MaxSessionDuration:       aws.Int64(minMaxSessionDuration - 1),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorValidationError)
}

func TestCreateRole_MaxSessionDuration_TooLarge(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("long-session"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		MaxSessionDuration:       aws.Int64(maxMaxSessionDuration + 1),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorValidationError)
}

func TestCreateRole_Duplicate(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "dup-role")

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("dup-role"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}

func TestGetRole(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "get-role")

	out, err := svc.GetRole(testAccountID, &iam.GetRoleInput{
		RoleName: aws.String("get-role"),
	})
	require.NoError(t, err)
	assert.Equal(t, "get-role", *out.Role.RoleName)
	assert.Equal(t, validTrustPolicy(), *out.Role.AssumeRolePolicyDocument)
}

func TestGetRole_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetRole(testAccountID, &iam.GetRoleInput{
		RoleName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListRoles(t *testing.T) {
	svc := setupTestIAMService(t)

	createTestRole(t, svc, "role1")
	createTestRole(t, svc, "role2")
	createTestRole(t, svc, "role3")

	out, err := svc.ListRoles(testAccountID, &iam.ListRolesInput{})
	require.NoError(t, err)
	assert.Len(t, out.Roles, 3)

	names := make(map[string]bool)
	for _, r := range out.Roles {
		names[*r.RoleName] = true
	}
	assert.True(t, names["role1"])
	assert.True(t, names["role2"])
	assert.True(t, names["role3"])
}

func TestListRoles_Empty(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.ListRoles(testAccountID, &iam.ListRolesInput{})
	require.NoError(t, err)
	assert.Len(t, out.Roles, 0)
}

func TestListRoles_PathPrefix(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("svc-role"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		Path:                     aws.String("/service-roles/"),
	})
	require.NoError(t, err)

	_, err = svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("admin-role"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		Path:                     aws.String("/admins/"),
	})
	require.NoError(t, err)

	out, err := svc.ListRoles(testAccountID, &iam.ListRolesInput{
		PathPrefix: aws.String("/service-roles/"),
	})
	require.NoError(t, err)
	assert.Len(t, out.Roles, 1)
	assert.Equal(t, "svc-role", *out.Roles[0].RoleName)
}

func TestDeleteRole(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "del-role")

	_, err := svc.DeleteRole(testAccountID, &iam.DeleteRoleInput{
		RoleName: aws.String("del-role"),
	})
	require.NoError(t, err)

	_, err = svc.GetRole(testAccountID, &iam.GetRoleInput{
		RoleName: aws.String("del-role"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteRole_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeleteRole(testAccountID, &iam.DeleteRoleInput{
		RoleName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteRole_WithAttachedPolicies(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "attached-role")
	policy := createTestPolicy(t, svc, "RolePolicy")

	_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DeleteRole(testAccountID, &iam.DeleteRoleInput{
		RoleName: role.RoleName,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

func TestDeleteRole_InInstanceProfile(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "inprofile-role")

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("inprofile"),
	})
	require.NoError(t, err)

	_, err = svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("inprofile"),
		RoleName:            aws.String("inprofile-role"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteRole(testAccountID, &iam.DeleteRoleInput{
		RoleName: aws.String("inprofile-role"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

func TestUpdateRole(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "upd-role")

	_, err := svc.UpdateRole(testAccountID, &iam.UpdateRoleInput{
		RoleName:           aws.String("upd-role"),
		Description:        aws.String("updated description"),
		MaxSessionDuration: aws.Int64(14400),
	})
	require.NoError(t, err)

	out, err := svc.GetRole(testAccountID, &iam.GetRoleInput{
		RoleName: aws.String("upd-role"),
	})
	require.NoError(t, err)
	assert.Equal(t, "updated description", *out.Role.Description)
	assert.Equal(t, int64(14400), *out.Role.MaxSessionDuration)
}

func TestUpdateRole_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.UpdateRole(testAccountID, &iam.UpdateRoleInput{
		RoleName:    aws.String("ghost"),
		Description: aws.String("never"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestUpdateRole_InvalidMaxSessionDuration(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "session-role")

	_, err := svc.UpdateRole(testAccountID, &iam.UpdateRoleInput{
		RoleName:           aws.String("session-role"),
		MaxSessionDuration: aws.Int64(60),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorValidationError)
}

func TestUpdateAssumeRolePolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "trust-role")

	newDoc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	_, err := svc.UpdateAssumeRolePolicy(testAccountID, &iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String("trust-role"),
		PolicyDocument: aws.String(newDoc),
	})
	require.NoError(t, err)

	out, err := svc.GetRole(testAccountID, &iam.GetRoleInput{
		RoleName: aws.String("trust-role"),
	})
	require.NoError(t, err)
	assert.Equal(t, newDoc, *out.Role.AssumeRolePolicyDocument)
}

func TestUpdateAssumeRolePolicy_Malformed(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "trust-role-malformed")

	_, err := svc.UpdateAssumeRolePolicy(testAccountID, &iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String("trust-role-malformed"),
		PolicyDocument: aws.String(`{bogus`),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestUpdateAssumeRolePolicy_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.UpdateAssumeRolePolicy(testAccountID, &iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String("ghost"),
		PolicyDocument: aws.String(validTrustPolicy()),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

// ============================================================================
// Role Policy Attachment Tests
// ============================================================================

func TestAttachRolePolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "attachtarget")
	policy := createTestPolicy(t, svc, "AttachPolicy")

	_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	out, err := svc.ListAttachedRolePolicies(testAccountID, &iam.ListAttachedRolePoliciesInput{
		RoleName: role.RoleName,
	})
	require.NoError(t, err)
	require.Len(t, out.AttachedPolicies, 1)
	assert.Equal(t, *policy.Arn, *out.AttachedPolicies[0].PolicyArn)
	assert.Equal(t, "AttachPolicy", *out.AttachedPolicies[0].PolicyName)
}

func TestAttachRolePolicy_Idempotent(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "idempotent")
	policy := createTestPolicy(t, svc, "IdempotentPolicy")

	_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	_, err = svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	out, err := svc.ListAttachedRolePolicies(testAccountID, &iam.ListAttachedRolePoliciesInput{
		RoleName: role.RoleName,
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 1, "duplicate attach should not double-count")
}

func TestAttachRolePolicy_PolicyNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "needspolicy")

	_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  aws.String("needspolicy"),
		PolicyArn: aws.String("arn:aws:iam::" + testAccountID + ":policy/Ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestAttachRolePolicy_RoleNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "OrphanPolicy")

	_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  aws.String("ghost"),
		PolicyArn: policy.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDetachRolePolicy(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "detach-role")
	policy := createTestPolicy(t, svc, "DetachPolicy")

	_, err := svc.AttachRolePolicy(testAccountID, &iam.AttachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	_, err = svc.DetachRolePolicy(testAccountID, &iam.DetachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	require.NoError(t, err)

	out, err := svc.ListAttachedRolePolicies(testAccountID, &iam.ListAttachedRolePoliciesInput{
		RoleName: role.RoleName,
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 0)
}

func TestDetachRolePolicy_NotAttached(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "detach-empty")
	policy := createTestPolicy(t, svc, "NotAttachedPolicy")

	_, err := svc.DetachRolePolicy(testAccountID, &iam.DetachRolePolicyInput{
		RoleName:  role.RoleName,
		PolicyArn: policy.Arn,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListAttachedRolePolicies_Empty(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "empty-attach")

	out, err := svc.ListAttachedRolePolicies(testAccountID, &iam.ListAttachedRolePoliciesInput{
		RoleName: role.RoleName,
	})
	require.NoError(t, err)
	assert.Len(t, out.AttachedPolicies, 0)
}

// ============================================================================
// Account Scoping
// ============================================================================

func TestRoles_AccountScoping(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Org B")
	require.NoError(t, err)

	// Same role name in two accounts should not collide.
	_, err = svc.CreateRole(accA.AccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("shared-name"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	require.NoError(t, err)

	_, err = svc.CreateRole(accB.AccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("shared-name"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
	})
	require.NoError(t, err)

	// Each account sees only its own role.
	listA, err := svc.ListRoles(accA.AccountID, &iam.ListRolesInput{})
	require.NoError(t, err)
	require.Len(t, listA.Roles, 1)
	assert.Contains(t, *listA.Roles[0].Arn, accA.AccountID)

	listB, err := svc.ListRoles(accB.AccountID, &iam.ListRolesInput{})
	require.NoError(t, err)
	require.Len(t, listB.Roles, 1)
	assert.Contains(t, *listB.Roles[0].Arn, accB.AccountID)

	// Account A cannot delete Account B's role with the same name — wait,
	// they share the name but live under different KV prefixes. Deleting in
	// A leaves B intact.
	_, err = svc.DeleteRole(accA.AccountID, &iam.DeleteRoleInput{
		RoleName: aws.String("shared-name"),
	})
	require.NoError(t, err)

	_, err = svc.GetRole(accB.AccountID, &iam.GetRoleInput{
		RoleName: aws.String("shared-name"),
	})
	require.NoError(t, err, "Account B's role must survive Account A's delete")
}
