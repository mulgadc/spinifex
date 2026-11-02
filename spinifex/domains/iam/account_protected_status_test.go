package iam

import (
	"testing"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Control-plane credentials for the system and bootstrap-admin accounts skip the
// account-admission check downstream, so IAM must never let either leave ACTIVE.
func TestSetAccountStatusRefusesToMoveAProtectedAccountOffActive(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	systemSecret, err := svc.key.EncryptBase64("system-secret")
	require.NoError(t, err)
	adminSecret, err := svc.key.EncryptBase64("admin-secret")
	require.NoError(t, err)
	require.NoError(t, svc.SeedBootstrap(&BootstrapData{
		AccessKeyID:     "AKIASYSTEM1234567890",
		EncryptedSecret: systemSecret,
		AccountID:       awsidentifiers.GlobalAccountID,
		Admin: &AdminBootstrapData{
			AccountID:       "000000000001",
			AccountName:     "spinifex",
			UserName:        "admin",
			AccessKeyID:     "AKIAADMIN12345678901",
			EncryptedSecret: adminSecret,
		},
	}))

	for _, accountID := range []string{awsidentifiers.GlobalAccountID, "000000000001"} {
		before, err := svc.GetAccount(accountID)
		require.NoError(t, err)
		require.Equal(t, AccountStatusActive, before.Status)

		for _, status := range []string{AccountStatusSuspended, AccountStatusTerminating} {
			got, err := svc.SetAccountStatus(accountID, status)
			require.ErrorIs(t, err, ErrAccountUndeletable, "%s -> %s", accountID, status)
			assert.Nil(t, got)

			after, err := svc.GetAccount(accountID)
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused write must leave %s unchanged", accountID)
		}

		// Writing ACTIVE to an ACTIVE protected account is a no-op, not a refusal.
		got, err := svc.SetAccountStatus(accountID, AccountStatusActive)
		require.NoError(t, err)
		assert.Equal(t, before.Generation, got.Generation)
	}
}
