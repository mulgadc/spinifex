package accountteardown

//test:in-package — drives the real final-stage reapers against the real
// EC2 account-settings service to pin that teardown leaves the bucket alone.

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No reaper in this package references the EC2 account-settings bucket or
// calls a settings delete. Running the real engine and the real StageAccount
// reapers against an account with settings enabled shows the consequence:
// the settings record outlives the account record it belongs to.
func TestCurrentBehaviour_AccountTermination_EC2AccountSettingsSurviveTeardown(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := testCtx(t)

	settings, err := ec2account.NewAccountSettingsServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	_, err = settings.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	_, err = settings.EnableSerialConsoleAccess(ctx, &ec2.EnableSerialConsoleAccessInput{}, testAccountID)
	require.NoError(t, err)

	accounts := newFakeAccounts()
	iam := &accountIAM{account: &handlers_iam.Account{AccountID: testAccountID, AccountName: testAccountName}}
	engine := quietEngine(accounts, AccountReapers(iam, nil, nil)...)

	result, err := engine.Teardown(ctx, request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.True(t, accounts.deleted)
	assert.Zero(t, result.StuckCount())

	bucket, err := js.KeyValue(ctx, ec2account.KVBucketAccountSettings)
	require.NoError(t, err)
	_, err = bucket.Get(ctx, testAccountID)
	require.NoError(t, err, "the settings key must still be present after the account is torn down")

	out, err := settings.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)
	assert.True(t, *out.EbsEncryptionByDefault)
}
