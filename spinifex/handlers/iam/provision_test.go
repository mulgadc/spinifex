package handlers_iam_test

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const provisionAccountID = "000000000042"

func newIAMService(t *testing.T) *handlers_iam.IAMServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	svc, err := handlers_iam.NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)
	return svc
}

func TestNormalizeAccountName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases", "Ben@Example.COM", "ben@example.com"},
		{"trims", "  ben@example.com\t", "ben@example.com"},
		{"already normal", "ben@example.com", "ben@example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, handlers_iam.NormalizeAccountName(tc.in))
		})
	}
}

func TestProvisionAccountCreatesAdminIdentity(t *testing.T) {
	t.Parallel()
	svc := newIAMService(t)

	got, err := handlers_iam.ProvisionAccount(svc, provisionAccountID, "ben@example.com")
	require.NoError(t, err)

	assert.Equal(t, provisionAccountID, got.AccountID)
	assert.Equal(t, handlers_iam.AdminUserName, got.AdminUser)
	assert.NotEmpty(t, got.AccessKeyID)
	assert.NotEmpty(t, got.SecretAccessKey)

	attached, err := svc.ListAttachedUserPolicies(provisionAccountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(handlers_iam.AdminUserName),
	})
	require.NoError(t, err)
	require.Len(t, attached.AttachedPolicies, 1)
	assert.Equal(t, handlers_iam.AdminPolicyName, aws.StringValue(attached.AttachedPolicies[0].PolicyName))
}

// A retry must finish the job rather than fail on the steps that already ran:
// account creation has no rollback, so resuming is the only recovery path.
func TestProvisionAccountResumesPartialState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, svc handlers_iam.IAMService)
	}{
		{
			name: "admin user already exists",
			prepare: func(t *testing.T, svc handlers_iam.IAMService) {
				_, err := svc.CreateUser(provisionAccountID, &iam.CreateUserInput{
					UserName: aws.String(handlers_iam.AdminUserName),
				})
				require.NoError(t, err)
			},
		},
		{
			name: "admin user and policy already exist",
			prepare: func(t *testing.T, svc handlers_iam.IAMService) {
				_, err := svc.CreateUser(provisionAccountID, &iam.CreateUserInput{
					UserName: aws.String(handlers_iam.AdminUserName),
				})
				require.NoError(t, err)
				_, err = svc.CreatePolicy(provisionAccountID, &iam.CreatePolicyInput{
					PolicyName:     aws.String(handlers_iam.AdminPolicyName),
					PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
				})
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newIAMService(t)
			tc.prepare(t, svc)

			got, err := handlers_iam.ProvisionAccount(svc, provisionAccountID, "ben@example.com")
			require.NoError(t, err)
			assert.NotEmpty(t, got.AccessKeyID)
		})
	}
}

// A key from an abandoned attempt was never handed to anyone, so it must not
// survive alongside the one that is.
func TestProvisionAccountLeavesExactlyOneAccessKey(t *testing.T) {
	t.Parallel()
	svc := newIAMService(t)

	first, err := handlers_iam.ProvisionAccount(svc, provisionAccountID, "ben@example.com")
	require.NoError(t, err)

	second, err := handlers_iam.ProvisionAccount(svc, provisionAccountID, "ben@example.com")
	require.NoError(t, err)
	assert.NotEqual(t, first.AccessKeyID, second.AccessKeyID)

	listed, err := svc.ListAccessKeys(provisionAccountID, &iam.ListAccessKeysInput{
		UserName: aws.String(handlers_iam.AdminUserName),
	})
	require.NoError(t, err)
	require.Len(t, listed.AccessKeyMetadata, 1)
	assert.Equal(t, second.AccessKeyID, aws.StringValue(listed.AccessKeyMetadata[0].AccessKeyId))
}

func TestProvisionAccountRejectsEmptyInput(t *testing.T) {
	t.Parallel()
	svc := newIAMService(t)

	_, err := handlers_iam.ProvisionAccount(svc, "", "ben@example.com")
	assert.Error(t, err)

	_, err = handlers_iam.ProvisionAccount(svc, provisionAccountID, "")
	assert.Error(t, err)

	_, err = handlers_iam.ProvisionAccount(nil, provisionAccountID, "ben@example.com")
	assert.Error(t, err)
}
