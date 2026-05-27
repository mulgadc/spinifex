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

func createTestInstanceProfile(t *testing.T, svc *IAMServiceImpl, name string) *iam.InstanceProfile {
	t.Helper()
	out, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(name),
	})
	require.NoError(t, err)
	return out.InstanceProfile
}

// ============================================================================
// InstanceProfile CRUD Tests
// ============================================================================

func TestCreateInstanceProfile(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("app-profile"),
		Path:                aws.String("/service-profiles/"),
		Tags: []*iam.Tag{
			{Key: aws.String("env"), Value: aws.String("prod")},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, out.InstanceProfile)
	assert.Equal(t, "app-profile", *out.InstanceProfile.InstanceProfileName)
	assert.Equal(t, "/service-profiles/", *out.InstanceProfile.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":instance-profile/service-profiles/app-profile", *out.InstanceProfile.Arn)
	require.True(t, len(*out.InstanceProfile.InstanceProfileId) > 4)
	assert.Equal(t, "AIPA", (*out.InstanceProfile.InstanceProfileId)[:4])
	assert.Empty(t, out.InstanceProfile.Roles, "freshly created profile has no role attached")
	require.Len(t, out.InstanceProfile.Tags, 1)
	assert.Equal(t, "env", *out.InstanceProfile.Tags[0].Key)
}

func TestCreateInstanceProfile_DefaultPath(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("default-profile"),
	})
	require.NoError(t, err)
	assert.Equal(t, "/", *out.InstanceProfile.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":instance-profile/default-profile", *out.InstanceProfile.Arn)
}

func TestCreateInstanceProfile_InvalidName(t *testing.T) {
	svc := setupTestIAMService(t)
	longName := strings.Repeat("a", 129)

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(longName),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateInstanceProfile_InvalidPath(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("badpath"),
		Path:                aws.String("missing-slashes"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateInstanceProfile_Duplicate(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "dup-profile")

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("dup-profile"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}

func TestGetInstanceProfile(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "get-profile")

	out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("get-profile"),
	})
	require.NoError(t, err)
	assert.Equal(t, "get-profile", *out.InstanceProfile.InstanceProfileName)
	assert.Empty(t, out.InstanceProfile.Roles)
}

func TestGetInstanceProfile_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetInstanceProfile_WithAttachedRole(t *testing.T) {
	svc := setupTestIAMService(t)
	role := createTestRole(t, svc, "embedded-role")
	createTestInstanceProfile(t, svc, "wrap-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("wrap-profile"),
		RoleName:            role.RoleName,
	})
	require.NoError(t, err)

	out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("wrap-profile"),
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceProfile.Roles, 1)
	assert.Equal(t, "embedded-role", *out.InstanceProfile.Roles[0].RoleName)
}

func TestListInstanceProfiles(t *testing.T) {
	svc := setupTestIAMService(t)

	createTestInstanceProfile(t, svc, "profile1")
	createTestInstanceProfile(t, svc, "profile2")
	createTestInstanceProfile(t, svc, "profile3")

	out, err := svc.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{})
	require.NoError(t, err)
	assert.Len(t, out.InstanceProfiles, 3)

	names := make(map[string]bool)
	for _, p := range out.InstanceProfiles {
		names[*p.InstanceProfileName] = true
	}
	assert.True(t, names["profile1"])
	assert.True(t, names["profile2"])
	assert.True(t, names["profile3"])
}

func TestListInstanceProfiles_Empty(t *testing.T) {
	svc := setupTestIAMService(t)

	out, err := svc.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{})
	require.NoError(t, err)
	assert.Len(t, out.InstanceProfiles, 0)
}

func TestListInstanceProfiles_PathPrefix(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("svc-profile"),
		Path:                aws.String("/service-profiles/"),
	})
	require.NoError(t, err)

	_, err = svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("admin-profile"),
		Path:                aws.String("/admins/"),
	})
	require.NoError(t, err)

	out, err := svc.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{
		PathPrefix: aws.String("/service-profiles/"),
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceProfiles, 1)
	assert.Equal(t, "svc-profile", *out.InstanceProfiles[0].InstanceProfileName)
}

func TestDeleteInstanceProfile(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "del-profile")

	_, err := svc.DeleteInstanceProfile(testAccountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("del-profile"),
	})
	require.NoError(t, err)

	_, err = svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("del-profile"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteInstanceProfile_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.DeleteInstanceProfile(testAccountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteInstanceProfile_WithRoleAttached(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "attached-role")
	createTestInstanceProfile(t, svc, "loaded-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("loaded-profile"),
		RoleName:            aws.String("attached-role"),
	})
	require.NoError(t, err)

	_, err = svc.DeleteInstanceProfile(testAccountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("loaded-profile"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
}

// ============================================================================
// InstanceProfile ↔ Role Binding Tests
// ============================================================================

func TestAddRoleToInstanceProfile(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "binding-role")
	createTestInstanceProfile(t, svc, "binding-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("binding-profile"),
		RoleName:            aws.String("binding-role"),
	})
	require.NoError(t, err)

	out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("binding-profile"),
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceProfile.Roles, 1)
	assert.Equal(t, "binding-role", *out.InstanceProfile.Roles[0].RoleName)
}

func TestAddRoleToInstanceProfile_RoleNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "no-role-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("no-role-profile"),
		RoleName:            aws.String("ghost-role"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestAddRoleToInstanceProfile_ProfileNotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "lonely-role")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("ghost-profile"),
		RoleName:            aws.String("lonely-role"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestAddRoleToInstanceProfile_OneRoleLimit(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "first-role")
	createTestRole(t, svc, "second-role")
	createTestInstanceProfile(t, svc, "exclusive-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("exclusive-profile"),
		RoleName:            aws.String("first-role"),
	})
	require.NoError(t, err)

	// Second add should fail per AWS one-role-per-profile rule.
	_, err = svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("exclusive-profile"),
		RoleName:            aws.String("second-role"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)
}

func TestRemoveRoleFromInstanceProfile(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "remove-role")
	createTestInstanceProfile(t, svc, "remove-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("remove-profile"),
		RoleName:            aws.String("remove-role"),
	})
	require.NoError(t, err)

	_, err = svc.RemoveRoleFromInstanceProfile(testAccountID, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String("remove-profile"),
		RoleName:            aws.String("remove-role"),
	})
	require.NoError(t, err)

	out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("remove-profile"),
	})
	require.NoError(t, err)
	assert.Empty(t, out.InstanceProfile.Roles)
}

func TestRemoveRoleFromInstanceProfile_NoRoleAttached(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "empty-profile")

	_, err := svc.RemoveRoleFromInstanceProfile(testAccountID, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String("empty-profile"),
		RoleName:            aws.String("anything"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestRemoveRoleFromInstanceProfile_WrongRoleName(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "actual-role")
	createTestInstanceProfile(t, svc, "wrong-name-profile")

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("wrong-name-profile"),
		RoleName:            aws.String("actual-role"),
	})
	require.NoError(t, err)

	_, err = svc.RemoveRoleFromInstanceProfile(testAccountID, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String("wrong-name-profile"),
		RoleName:            aws.String("not-the-attached-role"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListInstanceProfilesForRole(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "popular-role")

	createTestInstanceProfile(t, svc, "profile-a")
	createTestInstanceProfile(t, svc, "profile-b")
	createTestInstanceProfile(t, svc, "profile-c") // not attached

	_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("profile-a"),
		RoleName:            aws.String("popular-role"),
	})
	require.NoError(t, err)
	_, err = svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("profile-b"),
		RoleName:            aws.String("popular-role"),
	})
	require.NoError(t, err)

	out, err := svc.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("popular-role"),
	})
	require.NoError(t, err)
	require.Len(t, out.InstanceProfiles, 2)
	names := map[string]bool{}
	for _, p := range out.InstanceProfiles {
		names[*p.InstanceProfileName] = true
	}
	assert.True(t, names["profile-a"])
	assert.True(t, names["profile-b"])
	assert.False(t, names["profile-c"])
}

func TestListInstanceProfilesForRole_RoleNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListInstanceProfilesForRole_None(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "unused-role")

	out, err := svc.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("unused-role"),
	})
	require.NoError(t, err)
	assert.Len(t, out.InstanceProfiles, 0)
}

// ============================================================================
// Account Scoping
// ============================================================================

// ============================================================================
// ResolveInstanceProfile
// ============================================================================

func TestResolveInstanceProfile_ByName(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestInstanceProfile(t, svc, "by-name-profile")

	profile, err := svc.ResolveInstanceProfile(testAccountID, "by-name-profile")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, "by-name-profile", profile.InstanceProfileName)
	assert.Equal(t, *created.Arn, profile.ARN)
}

func TestResolveInstanceProfile_ByARN(t *testing.T) {
	svc := setupTestIAMService(t)
	created := createTestInstanceProfile(t, svc, "by-arn-profile")

	profile, err := svc.ResolveInstanceProfile(testAccountID, *created.Arn)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, "by-arn-profile", profile.InstanceProfileName)
}

func TestResolveInstanceProfile_ByARNWithPath(t *testing.T) {
	svc := setupTestIAMService(t)
	out, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("nested-profile"),
		Path:                aws.String("/service-profiles/team-a/"),
	})
	require.NoError(t, err)

	profile, err := svc.ResolveInstanceProfile(testAccountID, *out.InstanceProfile.Arn)
	require.NoError(t, err)
	assert.Equal(t, "nested-profile", profile.InstanceProfileName)
	assert.Equal(t, "/service-profiles/team-a/", profile.Path)
}

func TestResolveInstanceProfile_CrossAccountARNRejected(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "shadow-profile")

	otherAccountARN := "arn:aws:iam::999999999999:instance-profile/shadow-profile"
	_, err := svc.ResolveInstanceProfile(testAccountID, otherAccountARN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorAccessDenied)
}

func TestResolveInstanceProfile_NameNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ResolveInstanceProfile(testAccountID, "ghost-profile")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestResolveInstanceProfile_ARNNotFound(t *testing.T) {
	svc := setupTestIAMService(t)

	arn := "arn:aws:iam::" + testAccountID + ":instance-profile/ghost"
	_, err := svc.ResolveInstanceProfile(testAccountID, arn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestResolveInstanceProfile_MalformedARN(t *testing.T) {
	svc := setupTestIAMService(t)

	cases := []string{
		"arn:aws:iam::" + testAccountID + ":role/not-a-profile",
		"arn:aws:iam::" + testAccountID + ":instance-profile/",
		"arn:aws:s3:::bucket/key",
		"arn:bogus",
	}
	for _, arn := range cases {
		_, err := svc.ResolveInstanceProfile(testAccountID, arn)
		require.Error(t, err, "expected error for %q", arn)
		assert.Contains(t, err.Error(), awserrors.ErrorInvalidIamInstanceProfileArnMalformed,
			"expected malformed-ARN error for %q", arn)
	}
}

func TestResolveInstanceProfile_EmptyReference(t *testing.T) {
	svc := setupTestIAMService(t)

	_, err := svc.ResolveInstanceProfile(testAccountID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestInstanceProfiles_AccountScoping(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Org B")
	require.NoError(t, err)

	_, err = svc.CreateInstanceProfile(accA.AccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("shared-profile"),
	})
	require.NoError(t, err)
	_, err = svc.CreateInstanceProfile(accB.AccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("shared-profile"),
	})
	require.NoError(t, err)

	listA, err := svc.ListInstanceProfiles(accA.AccountID, &iam.ListInstanceProfilesInput{})
	require.NoError(t, err)
	require.Len(t, listA.InstanceProfiles, 1)
	assert.Contains(t, *listA.InstanceProfiles[0].Arn, accA.AccountID)

	listB, err := svc.ListInstanceProfiles(accB.AccountID, &iam.ListInstanceProfilesInput{})
	require.NoError(t, err)
	require.Len(t, listB.InstanceProfiles, 1)
	assert.Contains(t, *listB.InstanceProfiles[0].Arn, accB.AccountID)
}
