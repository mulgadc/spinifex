package handlers_iam

import (
	"fmt"
	"strings"
	"sync"
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
	t.Parallel()
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
	require.Greater(t, len(*out.InstanceProfile.InstanceProfileId), 4)
	assert.Equal(t, "AIPA", (*out.InstanceProfile.InstanceProfileId)[:4])
	assert.Empty(t, out.InstanceProfile.Roles, "freshly created profile has no role attached")
	require.Len(t, out.InstanceProfile.Tags, 1)
	assert.Equal(t, "env", *out.InstanceProfile.Tags[0].Key)
}

func TestCreateInstanceProfile_DefaultPath(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	out, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("default-profile"),
	})
	require.NoError(t, err)
	assert.Equal(t, "/", *out.InstanceProfile.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":instance-profile/default-profile", *out.InstanceProfile.Arn)
}

func TestCreateInstanceProfile_InvalidName(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	longName := strings.Repeat("a", 129)

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(longName),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateInstanceProfile_InvalidPath(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("badpath"),
		Path:                aws.String("missing-slashes"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestCreateInstanceProfile_Duplicate(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "dup-profile")

	_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("dup-profile"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMEntityAlreadyExists)
}

func TestGetInstanceProfile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestGetInstanceProfile_WithAttachedRole(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	svc := setupTestIAMService(t)

	out, err := svc.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{})
	require.NoError(t, err)
	assert.Empty(t, out.InstanceProfiles)
}

func TestListInstanceProfiles_PathPrefix(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.DeleteInstanceProfile(testAccountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteInstanceProfile_WithRoleAttached(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// raceRounds is how many times a concurrency test replays its race on a fresh
// profile. One round can serialize and miss a lost update; a handful makes a
// reintroduced blind Put fail reliably.
const raceRounds = 8

// runConcurrently calls fn n times in parallel, released together so the calls
// overlap rather than serializing, and returns each call's error by index.
func runConcurrently(n int, fn func(i int) error) []error {
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// TestAddRoleToInstanceProfile_ConcurrentDistinctRoles pins the one-role
// limit under concurrency: a blind read-modify-Put let both callers pass the
// guard from the same revision, so the later write silently replaced the first
// role. Exactly one attach may win; the loser must see LimitExceeded.
func TestAddRoleToInstanceProfile_ConcurrentDistinctRoles(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	roles := []string{"race-role-a", "race-role-b"}
	for _, r := range roles {
		createTestRole(t, svc, r)
	}

	for round := range raceRounds {
		profileName := fmt.Sprintf("race-profile-%d", round)
		createTestInstanceProfile(t, svc, profileName)

		errs := runConcurrently(len(roles), func(i int) error {
			_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				RoleName:            aws.String(roles[i]),
			})
			return err
		})

		var winner string
		for i, err := range errs {
			if err == nil {
				require.Emptyf(t, winner, "both attaches succeeded: %v and %v", winner, roles[i])
				winner = roles[i]
				continue
			}
			assert.Containsf(t, err.Error(), awserrors.ErrorIAMLimitExceeded, "attach %s", roles[i])
		}
		require.NotEmptyf(t, winner, "no attach succeeded: %v", errs)

		out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
		})
		require.NoError(t, err)
		require.Len(t, out.InstanceProfile.Roles, 1)
		assert.Equal(t, winner, *out.InstanceProfile.Roles[0].RoleName, "the profile must carry the attach that won")
	}
}

// TestRemoveRoleFromInstanceProfile_ConcurrentDetach is the mirror case: two
// callers detach the same role at once and exactly one may win, the loser
// re-reading an empty profile and reporting NoSuchEntity.
func TestRemoveRoleFromInstanceProfile_ConcurrentDetach(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "detach-race-role")

	for round := range raceRounds {
		profileName := fmt.Sprintf("detach-race-profile-%d", round)
		createTestInstanceProfile(t, svc, profileName)
		_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
			RoleName:            aws.String("detach-race-role"),
		})
		require.NoError(t, err)

		errs := runConcurrently(2, func(int) error {
			_, err := svc.RemoveRoleFromInstanceProfile(testAccountID, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				RoleName:            aws.String("detach-race-role"),
			})
			return err
		})

		successes := 0
		for _, err := range errs {
			if err == nil {
				successes++
				continue
			}
			assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
		}
		assert.Equalf(t, 1, successes, "exactly one detach may succeed: %v", errs)

		out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
		})
		require.NoError(t, err)
		assert.Empty(t, out.InstanceProfile.Roles)
	}
}

// TestTagInstanceProfile_ConcurrentWithAttach pins the tag path against the
// same lost update: a blind Put wrote back a stale RoleName and silently
// undid a role attach that had already reported success.
func TestTagInstanceProfile_ConcurrentWithAttach(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "tag-race-role")

	for round := range raceRounds {
		profileName := fmt.Sprintf("tag-race-profile-%d", round)
		createTestInstanceProfile(t, svc, profileName)

		errs := runConcurrently(2, func(i int) error {
			if i == 0 {
				_, err := svc.AddRoleToInstanceProfile(testAccountID, &iam.AddRoleToInstanceProfileInput{
					InstanceProfileName: aws.String(profileName),
					RoleName:            aws.String("tag-race-role"),
				})
				return err
			}
			_, err := svc.TagInstanceProfile(testAccountID, &iam.TagInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				Tags:                []*iam.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
			})
			return err
		})

		require.NoError(t, errs[0], "attach")
		require.NoError(t, errs[1], "tag")

		out, err := svc.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
		})
		require.NoError(t, err)
		require.Len(t, out.InstanceProfile.Roles, 1, "tagging must not drop the attached role")
		require.Len(t, out.InstanceProfile.Tags, 1, "the attach must not drop the tag")
	}
}

// TestTagInstanceProfile_ConcurrentDistinctKeys pins tag writes against each
// other: under a blind Put all but one of the racing keys were lost.
func TestTagInstanceProfile_ConcurrentDistinctKeys(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "tag-keys-profile")

	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	errs := runConcurrently(len(keys), func(i int) error {
		_, err := svc.TagInstanceProfile(testAccountID, &iam.TagInstanceProfileInput{
			InstanceProfileName: aws.String("tag-keys-profile"),
			Tags:                []*iam.Tag{{Key: aws.String(keys[i]), Value: aws.String("v")}},
		})
		return err
	})

	for i, err := range errs {
		require.NoErrorf(t, err, "tag %s", keys[i])
	}

	out, err := svc.ListInstanceProfileTags(testAccountID, &iam.ListInstanceProfileTagsInput{
		InstanceProfileName: aws.String("tag-keys-profile"),
	})
	require.NoError(t, err)
	got := make([]string, 0, len(out.Tags))
	for _, tag := range out.Tags {
		got = append(got, *tag.Key)
	}
	assert.ElementsMatch(t, keys, got, "all concurrently-written tags must persist")
}

func TestRemoveRoleFromInstanceProfile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("ghost"),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestListInstanceProfilesForRole_None(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestRole(t, svc, "unused-role")

	out, err := svc.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String("unused-role"),
	})
	require.NoError(t, err)
	assert.Empty(t, out.InstanceProfiles)
}

// ============================================================================
// Account Scoping
// ============================================================================

// ============================================================================
// ResolveInstanceProfile
// ============================================================================

func TestResolveInstanceProfile_ByName(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	created := createTestInstanceProfile(t, svc, "by-name-profile")

	profile, err := svc.ResolveInstanceProfile(testAccountID, "by-name-profile")
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, "by-name-profile", profile.InstanceProfileName)
	assert.Equal(t, *created.Arn, profile.ARN)
}

func TestResolveInstanceProfile_ByARN(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	created := createTestInstanceProfile(t, svc, "by-arn-profile")

	profile, err := svc.ResolveInstanceProfile(testAccountID, *created.Arn)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, "by-arn-profile", profile.InstanceProfileName)
}

func TestResolveInstanceProfile_ByARNWithPath(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestInstanceProfile(t, svc, "shadow-profile")

	otherAccountARN := "arn:aws:iam::999999999999:instance-profile/shadow-profile"
	_, err := svc.ResolveInstanceProfile(testAccountID, otherAccountARN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorAccessDenied)
}

func TestResolveInstanceProfile_NameNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.ResolveInstanceProfile(testAccountID, "ghost-profile")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestResolveInstanceProfile_ARNNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	arn := "arn:aws:iam::" + testAccountID + ":instance-profile/ghost"
	_, err := svc.ResolveInstanceProfile(testAccountID, arn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestResolveInstanceProfile_MalformedARN(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.ResolveInstanceProfile(testAccountID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMInvalidInput)
}

func TestInstanceProfiles_AccountScoping(t *testing.T) {
	t.Parallel()
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
