package accountlifecycle

//test:in-package — drives the real final-stage reapers and the seal reaper against the real quota service to pin that teardown seals the override record.

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serviceSealer seals through the real quota service, standing in for the
// awsgw responder that owns the override record.
type serviceSealer struct{ svc *quota.Service }

func (s serviceSealer) SealAccountQuotaOverride(ctx context.Context, accountID string) (uint64, error) {
	seal, err := s.svc.SealAccountOverrides(ctx, accountID)
	if err != nil {
		return 0, err
	}
	return seal.Revision, nil
}

// Running the real engine with the seal reaper and the real StageAccount
// reapers against an account with a raised vCPU override: the usage counter is
// removed as before, and the override record outlives the account only as a
// sealed tombstone that no writer can change or recreate.
func TestAccountTermination_QuotaOverrideEndsSealed(t *testing.T) {
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
	svc.SetAccountTerminationChecker(alwaysTerminatingChecker{})
	unlimited := quota.Unlimited
	require.NoError(t, svc.PutAccountQuota(ctx, testAccountID, quota.Overrides{VCPUs: &unlimited}, "operator"))

	accounts := newFakeAccounts()
	iam := &accountIAM{account: &iamdomain.Account{AccountID: testAccountID, AccountName: testAccountName}}
	reapers := append([]Reaper{&quotaOverrideSealReaper{
		sealer:     serviceSealer{svc: svc},
		capability: &fakeSealCapability{report: completeReport("gw-1")},
	}}, AccountReapers(iam, nil, usage)...)
	engine := quietEngine(accounts, reapers...)

	result, err := engine.Teardown(ctx, request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.True(t, accounts.deleted)
	assert.Zero(t, result.StuckCount())

	_, err = usage.Get(ctx, testAccountID)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "teardown's quota-usage reaper must remove the vCPU counter")

	_, err = overridesKV.Get(ctx, testAccountID)
	require.NoError(t, err, "the sealed record stays as the tombstone")

	err = svc.PutAccountQuota(ctx, testAccountID, quota.Overrides{VCPUs: &unlimited}, "operator")
	require.Error(t, err, "a sealed record must refuse a later override write")
	_, _, err = svc.GetAccountQuota(ctx, testAccountID)
	require.Error(t, err, "a sealed record must not yield the old override")
}
