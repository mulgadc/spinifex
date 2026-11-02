package iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntityErrorMessages pins the per-case text AWS returns for NoSuchEntity,
// EntityAlreadyExists, DeleteConflict and LimitExceeded, as recorded against
// real IAM, so a client sees which entity is missing or in conflict.
func TestEntityErrorMessages(t *testing.T) {
	svc := setupTestIAMService(t)
	acct := testAccountID

	createTestUser(t, svc, "u1")
	createTestRole(t, svc, "r1")
	createTestRole(t, svc, "r2")
	createTestGroup(t, svc, "g1")
	createTestInstanceProfile(t, svc, "ip1")
	policy := createTestPolicy(t, svc, "p1")
	policyARN := aws.StringValue(policy.Arn)
	missingPolicyARN := "arn:aws:iam::" + acct + ":policy/p1-nope"
	_, err := svc.CreateOpenIDConnectProvider(acct, &iam.CreateOpenIDConnectProviderInput{
		Url: aws.String(testIssuer), ClientIDList: aws.StringSlice([]string{"sts.amazonaws.com"}),
	})
	require.NoError(t, err)
	missingOIDCARN := "arn:aws:iam::" + acct + ":oidc-provider/oidc.nope.example.com"

	// Conflict fixtures: a user holding a key, a role with a managed policy, a
	// role and profile linked to each other, and a group with a member.
	createTestUser(t, svc, "u-key")
	_, err = svc.CreateAccessKey(acct, &iam.CreateAccessKeyInput{UserName: aws.String("u-key")})
	require.NoError(t, err)
	createTestRole(t, svc, "r-pol")
	_, err = svc.AttachRolePolicy(acct, &iam.AttachRolePolicyInput{RoleName: aws.String("r-pol"), PolicyArn: aws.String(policyARN)})
	require.NoError(t, err)
	createTestRole(t, svc, "r-ip")
	createTestInstanceProfile(t, svc, "ip-r")
	_, err = svc.AddRoleToInstanceProfile(acct, &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("ip-r"), RoleName: aws.String("r-ip")})
	require.NoError(t, err)
	createTestGroup(t, svc, "g-mem")
	_, err = svc.AddUserToGroup(acct, &iam.AddUserToGroupInput{GroupName: aws.String("g-mem"), UserName: aws.String("u1")})
	require.NoError(t, err)

	cases := []struct {
		name     string
		call     func() error
		wantCode string
		wantMsg  string
	}{
		{"GetUser missing", func() error {
			_, err := svc.GetUser(acct, &iam.GetUserInput{UserName: aws.String("nope")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The user with name nope cannot be found."},
		{"TagUser missing", func() error {
			_, err := svc.TagUser(acct, &iam.TagUserInput{UserName: aws.String("nope"), Tags: []*iam.Tag{{Key: aws.String("k"), Value: aws.String("v")}}})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The user with name nope cannot be found."},
		{"AddUserToGroup missing user", func() error {
			_, err := svc.AddUserToGroup(acct, &iam.AddUserToGroupInput{GroupName: aws.String("g1"), UserName: aws.String("nope")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The user with name nope cannot be found."},
		{"GetRole missing", func() error {
			_, err := svc.GetRole(acct, &iam.GetRoleInput{RoleName: aws.String("nope")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The role with name nope cannot be found."},
		{"TagRole missing", func() error {
			_, err := svc.TagRole(acct, &iam.TagRoleInput{RoleName: aws.String("nope"), Tags: []*iam.Tag{{Key: aws.String("k"), Value: aws.String("v")}}})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The role with name nope cannot be found."},
		{"DeleteGroup missing", func() error {
			_, err := svc.DeleteGroup(acct, &iam.DeleteGroupInput{GroupName: aws.String("nope")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The group with name nope cannot be found."},
		{"GetPolicy missing", func() error {
			_, err := svc.GetPolicy(acct, &iam.GetPolicyInput{PolicyArn: aws.String(missingPolicyARN)})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "Policy " + missingPolicyARN + " was not found."},
		{"DeletePolicy missing", func() error {
			_, err := svc.DeletePolicy(acct, &iam.DeletePolicyInput{PolicyArn: aws.String(missingPolicyARN)})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "Policy " + missingPolicyARN + " was not found."},
		{"DetachUserPolicy not attached", func() error {
			_, err := svc.DetachUserPolicy(acct, &iam.DetachUserPolicyInput{UserName: aws.String("u1"), PolicyArn: aws.String(policyARN)})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "Policy " + policyARN + " was not found."},
		{"AttachRolePolicy missing policy", func() error {
			_, err := svc.AttachRolePolicy(acct, &iam.AttachRolePolicyInput{RoleName: aws.String("r1"), PolicyArn: aws.String(missingPolicyARN)})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "Policy " + missingPolicyARN + " does not exist or is not attachable."},
		{"GetInstanceProfile missing", func() error {
			_, err := svc.GetInstanceProfile(acct, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("nope")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "Instance Profile nope cannot be found."},
		{"RemoveRoleFromInstanceProfile not in profile", func() error {
			_, err := svc.RemoveRoleFromInstanceProfile(acct, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String("ip1"), RoleName: aws.String("r2")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "Role r2 in Instance Profile ip1 cannot be found."},
		{"GetOpenIDConnectProvider missing", func() error {
			_, err := svc.GetOpenIDConnectProvider(acct, &iam.GetOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(missingOIDCARN)})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "OpenIDConnect Provider not found for arn " + missingOIDCARN},
		{"GetUserPolicy missing", func() error {
			_, err := svc.GetUserPolicy(acct, &iam.GetUserPolicyInput{UserName: aws.String("u1"), PolicyName: aws.String("inl")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The user policy with name inl cannot be found."},
		{"DeleteRolePolicy missing", func() error {
			_, err := svc.DeleteRolePolicy(acct, &iam.DeleteRolePolicyInput{RoleName: aws.String("r1"), PolicyName: aws.String("rinl")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The role policy with name rinl cannot be found."},
		{"DeleteAccessKey missing", func() error {
			_, err := svc.DeleteAccessKey(acct, &iam.DeleteAccessKeyInput{UserName: aws.String("u1"), AccessKeyId: aws.String("AKIANOPE")})
			return err
		}, awserrors.ErrorIAMNoSuchEntity, "The Access Key with id AKIANOPE cannot be found."},

		{"CreateUser duplicate", func() error {
			_, err := svc.CreateUser(acct, &iam.CreateUserInput{UserName: aws.String("u1")})
			return err
		}, awserrors.ErrorIAMEntityAlreadyExists, "User with name u1 already exists."},
		{"CreateRole duplicate", func() error {
			_, err := svc.CreateRole(acct, &iam.CreateRoleInput{RoleName: aws.String("r1"), AssumeRolePolicyDocument: aws.String(validTrustPolicy())})
			return err
		}, awserrors.ErrorIAMEntityAlreadyExists, "Role with name r1 already exists."},
		{"CreateGroup duplicate", func() error {
			_, err := svc.CreateGroup(acct, &iam.CreateGroupInput{GroupName: aws.String("g1")})
			return err
		}, awserrors.ErrorIAMEntityAlreadyExists, "Group with name g1 already exists."},
		{"CreatePolicy duplicate", func() error {
			_, err := svc.CreatePolicy(acct, &iam.CreatePolicyInput{PolicyName: aws.String("p1"), PolicyDocument: aws.String(validPolicyDocument())})
			return err
		}, awserrors.ErrorIAMEntityAlreadyExists, "A policy called p1 already exists. Duplicate names are not allowed."},
		{"CreateInstanceProfile duplicate", func() error {
			_, err := svc.CreateInstanceProfile(acct, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("ip1")})
			return err
		}, awserrors.ErrorIAMEntityAlreadyExists, "Instance Profile ip1 already exists."},
		{"CreateOpenIDConnectProvider duplicate", func() error {
			_, err := svc.CreateOpenIDConnectProvider(acct, &iam.CreateOpenIDConnectProviderInput{
				Url: aws.String(testIssuer), ClientIDList: aws.StringSlice([]string{"sts.amazonaws.com"}),
			})
			return err
		}, awserrors.ErrorIAMEntityAlreadyExists, "Provider with url " + testIssuer + " already exists."},

		{"DeleteUser with access key", func() error {
			_, err := svc.DeleteUser(acct, &iam.DeleteUserInput{UserName: aws.String("u-key")})
			return err
		}, awserrors.ErrorIAMDeleteConflict, "Cannot delete entity, must delete access keys first."},
		{"DeleteRole with attached policy", func() error {
			_, err := svc.DeleteRole(acct, &iam.DeleteRoleInput{RoleName: aws.String("r-pol")})
			return err
		}, awserrors.ErrorIAMDeleteConflict, "Cannot delete entity, must detach all policies first."},
		{"DeleteRole in instance profile", func() error {
			_, err := svc.DeleteRole(acct, &iam.DeleteRoleInput{RoleName: aws.String("r-ip")})
			return err
		}, awserrors.ErrorIAMDeleteConflict, "Cannot delete entity, must remove roles from instance profile first."},
		{"DeleteInstanceProfile with role", func() error {
			_, err := svc.DeleteInstanceProfile(acct, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("ip-r")})
			return err
		}, awserrors.ErrorIAMDeleteConflict, "Cannot delete entity, must remove roles from instance profile first."},
		{"DeleteGroup with member", func() error {
			_, err := svc.DeleteGroup(acct, &iam.DeleteGroupInput{GroupName: aws.String("g-mem")})
			return err
		}, awserrors.ErrorIAMDeleteConflict, "Cannot delete entity, must remove users from group first."},
		{"DeletePolicy attached", func() error {
			_, err := svc.DeletePolicy(acct, &iam.DeletePolicyInput{PolicyArn: aws.String(policyARN)})
			return err
		}, awserrors.ErrorIAMDeleteConflict, "Cannot delete a policy attached to entities."},

		{"AddRoleToInstanceProfile second role", func() error {
			_, err := svc.AddRoleToInstanceProfile(acct, &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("ip-r"), RoleName: aws.String("r2")})
			return err
		}, awserrors.ErrorIAMLimitExceeded, "Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg, ok := awserrors.ResolveErrorDetail(tc.call())
			require.True(t, ok, "error carries no AWS code")
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantMsg, msg)
		})
	}
}
