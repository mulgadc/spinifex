package handlers_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// summaryCount reads a required, non-nil SummaryMap counter.
func summaryCount(t *testing.T, m map[string]*int64, key string) int64 {
	t.Helper()
	v, ok := m[key]
	require.True(t, ok, "SummaryMap missing key %q", key)
	require.NotNil(t, v, "SummaryMap key %q is nil", key)
	return *v
}

func seedResources(t *testing.T, svc *IAMServiceImpl, accountID string, users, groups, roles, policies, profiles int) {
	t.Helper()
	for i := range users {
		_, err := svc.CreateUser(accountID, &iam.CreateUserInput{UserName: aws.String(resName("user", i))})
		require.NoError(t, err)
	}
	for i := range groups {
		_, err := svc.CreateGroup(accountID, &iam.CreateGroupInput{GroupName: aws.String(resName("group", i))})
		require.NoError(t, err)
	}
	for i := range roles {
		_, err := svc.CreateRole(accountID, &iam.CreateRoleInput{
			RoleName:                 aws.String(resName("role", i)),
			AssumeRolePolicyDocument: aws.String(validTrustPolicy()),
		})
		require.NoError(t, err)
	}
	for i := range policies {
		_, err := svc.CreatePolicy(accountID, &iam.CreatePolicyInput{
			PolicyName:     aws.String(resName("policy", i)),
			PolicyDocument: aws.String(validPolicyDocument()),
		})
		require.NoError(t, err)
	}
	for i := range profiles {
		_, err := svc.CreateInstanceProfile(accountID, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(resName("profile", i)),
		})
		require.NoError(t, err)
	}
}

func resName(prefix string, i int) string {
	return prefix + string(rune('a'+i))
}

func TestGetAccountSummary_Counts(t *testing.T) {
	svc := setupTestIAMService(t)

	accA, err := svc.CreateAccount("Org A")
	require.NoError(t, err)
	accB, err := svc.CreateAccount("Org B")
	require.NoError(t, err)

	seedResources(t, svc, accA.AccountID, 2, 1, 3, 1, 2)
	// Account B holds different, larger counts that must not leak into A's summary.
	seedResources(t, svc, accB.AccountID, 4, 4, 4, 4, 4)

	out, err := svc.GetAccountSummary(accA.AccountID, &iam.GetAccountSummaryInput{})
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, int64(2), summaryCount(t, out.SummaryMap, "Users"))
	assert.Equal(t, int64(1), summaryCount(t, out.SummaryMap, "Groups"))
	assert.Equal(t, int64(3), summaryCount(t, out.SummaryMap, "Roles"))
	assert.Equal(t, int64(1), summaryCount(t, out.SummaryMap, "Policies"))
	assert.Equal(t, int64(2), summaryCount(t, out.SummaryMap, "InstanceProfiles"))

	// Quota values are static AWS-parity constants; AccessKeysPerUserQuota
	// tracks the maxAccessKeysPerUser enforcement constant.
	assert.Equal(t, int64(maxAccessKeysPerUser), summaryCount(t, out.SummaryMap, "AccessKeysPerUserQuota"))
	assert.Equal(t, int64(5000), summaryCount(t, out.SummaryMap, "UsersQuota"))

	// Resource types Spinifex does not model are reported as 0, not omitted.
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "MFADevices"))
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "AccountMFAEnabled"))
}

func TestGetAccountSummary_EmptyAccount(t *testing.T) {
	svc := setupTestIAMService(t)

	acc, err := svc.CreateAccount("Empty Org")
	require.NoError(t, err)

	out, err := svc.GetAccountSummary(acc.AccountID, &iam.GetAccountSummaryInput{})
	require.NoError(t, err)

	// Buckets contain only the "_version" migration key, which is never counted,
	// so an account with no resources reports zeroed counts rather than erroring.
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "Users"))
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "Groups"))
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "Roles"))
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "Policies"))
	assert.Equal(t, int64(0), summaryCount(t, out.SummaryMap, "InstanceProfiles"))

	// Quota parity constants are present even for an empty account.
	assert.Equal(t, int64(5000), summaryCount(t, out.SummaryMap, "UsersQuota"))
}
