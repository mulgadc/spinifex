package accountlifecycle

//test:in-package — drives the real final-stage reapers against the real quota service to pin that teardown leaves the override bucket alone.

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No reaper in this package references the quota override bucket or calls an
// override delete. Running the real engine and the real StageAccount reapers
// against an account with a raised vCPU override, with usage wired, shows the
// consequence: the override outlives the account record it belongs to, while
// the usage counter the reapers do own is removed.
func TestCurrentBehaviour_AccountTermination_QuotaOverridesSurviveTeardown(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := testCtx(t)

	usage, err := kvutil.GetOrCreateBucket(ctx, js, quota.KVBucketAccountUsage, 1)
	require.NoError(t, err)
	_, err = usage.Put(ctx, testAccountID, []byte(`{"vcpus":4}`))
	require.NoError(t, err)

	overridesKV, err := kvutil.GetOrCreateBucket(ctx, js, quota.KVBucketAccountQuota, 1)
	require.NoError(t, err)
	svc := quota.New(quota.Limits{Enabled: true, VCPUs: 16}, usage)
	svc.SetOverrides(overridesKV)
	unlimited := quota.Unlimited
	require.NoError(t, svc.PutAccountQuota(ctx, testAccountID, quota.Overrides{VCPUs: &unlimited}, "operator"))

	accounts := newFakeAccounts()
	iam := &accountIAM{account: &iamdomain.Account{AccountID: testAccountID, AccountName: testAccountName}}
	engine := quietEngine(accounts, AccountReapers(iam, nil, usage)...)

	result, err := engine.Teardown(ctx, request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.True(t, accounts.deleted)
	assert.Zero(t, result.StuckCount())

	// The reaper this package does own removed the usage counter, which shows
	// which store teardown actually clears.
	_, err = usage.Get(ctx, testAccountID)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "teardown's quota-usage reaper must remove the vCPU counter")

	_, err = overridesKV.Get(ctx, testAccountID)
	require.NoError(t, err, "the override key must still be present after the account is torn down")

	over, effective, err := svc.GetAccountQuota(ctx, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, over.VCPUs)
	assert.Equal(t, quota.Unlimited, *over.VCPUs)
	assert.Equal(t, quota.Unlimited, effective.VCPUs)
}
