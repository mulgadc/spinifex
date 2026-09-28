package handlers_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AWS returns a different field set for a role from each action that carries one.
func TestRoleShapes_PerAction(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	created, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("shaped"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		Description:              aws.String("described"),
		MaxSessionDuration:       aws.Int64(7200),
		Tags:                     []*iam.Tag{sdkTag("team", "core")},
	})
	require.NoError(t, err)
	assert.Nil(t, created.Role.Description, "CreateRole omits Description")
	assert.Nil(t, created.Role.MaxSessionDuration, "CreateRole omits MaxSessionDuration")
	assert.Nil(t, created.Role.RoleLastUsed)
	assert.Equal(t, map[string]string{"team": "core"}, tagsAsMap(created.Role.Tags))

	got, err := svc.GetRole(testAccountID, &iam.GetRoleInput{RoleName: aws.String("shaped")})
	require.NoError(t, err)
	assert.Equal(t, "described", aws.StringValue(got.Role.Description))
	assert.Equal(t, int64(7200), aws.Int64Value(got.Role.MaxSessionDuration))
	assert.Equal(t, map[string]string{"team": "core"}, tagsAsMap(got.Role.Tags))
	assert.Equal(t, &iam.RoleLastUsed{}, got.Role.RoleLastUsed, "an unused role has an empty RoleLastUsed")

	listed, err := svc.ListRoles(testAccountID, &iam.ListRolesInput{})
	require.NoError(t, err)
	require.Len(t, listed.Roles, 1)
	assert.Equal(t, "described", aws.StringValue(listed.Roles[0].Description))
	assert.Equal(t, int64(7200), aws.Int64Value(listed.Roles[0].MaxSessionDuration))
	assert.Nil(t, listed.Roles[0].Tags, "ListRoles omits Tags")
	assert.Nil(t, listed.Roles[0].RoleLastUsed, "ListRoles omits RoleLastUsed")
}

// A role inside an instance profile carries only its identity, trust policy and
// creation date; only GetInstanceProfile returns the profile's own Tags.
func TestInstanceProfileShapes_PerAction(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("embedded"),
		AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		Description:              aws.String("described"),
		Tags:                     []*iam.Tag{sdkTag("team", "core")},
	})
	require.NoError(t, err)
	_, err = svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("profile"),
		Tags:                []*iam.Tag{sdkTag("env", "prod")},
	})
	require.NoError(t, err)
	_, err = svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("profile"),
		RoleName:            aws.String("embedded"),
	})
	require.NoError(t, err)

	assertEmbeddedRole := func(t *testing.T, p *iam.InstanceProfile) {
		t.Helper()
		require.Len(t, p.Roles, 1)
		assert.Equal(t, &iam.Role{
			RoleName:                 aws.String("embedded"),
			RoleId:                   p.Roles[0].RoleId,
			Arn:                      aws.String("arn:aws:iam::" + testAccountID + ":role/embedded"),
			Path:                     aws.String("/"),
			AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
			CreateDate:               p.Roles[0].CreateDate,
		}, p.Roles[0])
	}

	got, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("profile")})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod"}, tagsAsMap(got.InstanceProfile.Tags))
	assertEmbeddedRole(t, got.InstanceProfile)

	listed, err := svc.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{})
	require.NoError(t, err)
	require.Len(t, listed.InstanceProfiles, 1)
	assert.Nil(t, listed.InstanceProfiles[0].Tags, "ListInstanceProfiles omits profile Tags")
	assertEmbeddedRole(t, listed.InstanceProfiles[0])

	forRole, err := svc.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String("embedded")})
	require.NoError(t, err)
	require.Len(t, forRole.InstanceProfiles, 1)
	assert.Nil(t, forRole.InstanceProfiles[0].Tags, "ListInstanceProfilesForRole omits profile Tags")
	assertEmbeddedRole(t, forRole.InstanceProfiles[0])
}

func TestCreateUser_EchoesTags(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	tagged, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{
		UserName: aws.String("tagged"),
		Tags:     []*iam.Tag{sdkTag("env", "prod")},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod"}, tagsAsMap(tagged.User.Tags))

	untagged, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{UserName: aws.String("untagged")})
	require.NoError(t, err)
	assert.Nil(t, untagged.User.Tags)
}

func TestPolicyShapes_PerAction(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	created, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName:     aws.String("shaped"),
		PolicyDocument: aws.String(validPolicyDocument()),
		Description:    aws.String("described"),
		Tags:           []*iam.Tag{sdkTag("team", "core")},
	})
	require.NoError(t, err)
	assert.Nil(t, created.Policy.Description, "CreatePolicy omits Description")
	assert.Equal(t, map[string]string{"team": "core"}, tagsAsMap(created.Policy.Tags))
	assert.Equal(t, int64(0), aws.Int64Value(created.Policy.PermissionsBoundaryUsageCount))
	require.NotNil(t, created.Policy.UpdateDate)
	assert.Equal(t, *created.Policy.CreateDate, *created.Policy.UpdateDate, "a single-version policy was updated when it was created")

	got, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{PolicyArn: created.Policy.Arn})
	require.NoError(t, err)
	assert.Equal(t, "described", aws.StringValue(got.Policy.Description))
	assert.Equal(t, map[string]string{"team": "core"}, tagsAsMap(got.Policy.Tags))
	assert.Equal(t, int64(0), aws.Int64Value(got.Policy.PermissionsBoundaryUsageCount))
	assert.Equal(t, *created.Policy.CreateDate, aws.TimeValue(got.Policy.UpdateDate))

	listed, err := svc.ListPolicies(testAccountID, &iam.ListPoliciesInput{})
	require.NoError(t, err)
	require.Len(t, listed.Policies, 1)
	assert.Nil(t, listed.Policies[0].Tags, "ListPolicies omits Tags")
	assert.Nil(t, listed.Policies[0].Description, "ListPolicies omits Description")
	assert.Equal(t, int64(0), aws.Int64Value(listed.Policies[0].PermissionsBoundaryUsageCount))
	assert.Equal(t, *created.Policy.CreateDate, aws.TimeValue(listed.Policies[0].UpdateDate))

	version, err := svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
		PolicyArn:      created.Policy.Arn,
		PolicyDocument: aws.String(validPolicyDocument()),
		SetAsDefault:   aws.Bool(true),
	})
	require.NoError(t, err)
	got, err = svc.GetPolicy(testAccountID, &iam.GetPolicyInput{PolicyArn: created.Policy.Arn})
	require.NoError(t, err)
	assert.Equal(t, *version.PolicyVersion.CreateDate, aws.TimeValue(got.Policy.UpdateDate),
		"UpdateDate follows the default version")
}
