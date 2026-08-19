//test:in-package — exercises the unexported limitsFor resolver against a real KV bucket.
package handlers_quota

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// newOverrideService builds a Service backed by a real JetStream KV bucket, so
// the resolver is exercised against the store it uses in production rather than
// a stand-in that cannot reproduce a read failure.
func newOverrideService(t *testing.T) (*Service, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  KVBucketAccountQuota,
		History: 1,
	})
	require.NoError(t, err)

	svc := New(baseLimits(), nil)
	svc.SetOverrides(kv)
	return svc, kv
}

// An account with no record is the normal state, and it must inherit the
// configured baseline without erroring.
func TestLimitsForAbsentKeyInherits(t *testing.T) {
	svc, _ := newOverrideService(t)

	got := svc.limitsFor(t.Context(), "123456789012")
	require.Equal(t, baseLimits(), got)
}

// The property the whole tier rests on: two accounts on one gateway resolve to
// different limits.
func TestLimitsForIsPerAccount(t *testing.T) {
	svc, _ := newOverrideService(t)
	const upgraded, sandbox = "000000000002", "000000000003"

	require.NoError(t, svc.PutAccountQuota(t.Context(), upgraded,
		Overrides{VCPUs: ptr(32), RDSInstances: ptr(8)}, "operator"))

	require.Equal(t, 32, svc.limitsFor(t.Context(), upgraded).VCPUs)
	require.Equal(t, 8, svc.limitsFor(t.Context(), upgraded).RDSInstances)
	require.Equal(t, baseLimits().VCPUs, svc.limitsFor(t.Context(), sandbox).VCPUs)
	require.Equal(t, baseLimits().RDSInstances, svc.limitsFor(t.Context(), sandbox).RDSInstances)
}

// A stored override must round-trip through KV with its provenance, and an
// unset dimension must still inherit.
func TestPutAndGetAccountQuota(t *testing.T) {
	svc, _ := newOverrideService(t)
	const account = "000000000002"

	require.NoError(t, svc.PutAccountQuota(t.Context(), account, Overrides{EIPs: ptr(12)}, "operator"))

	over, effective, err := svc.GetAccountQuota(t.Context(), account)
	require.NoError(t, err)
	require.NotNil(t, over.EIPs)
	require.Equal(t, 12, *over.EIPs)
	require.Equal(t, "operator", over.UpdatedBy)
	require.NotEmpty(t, over.UpdatedAt)
	require.Equal(t, 12, effective.EIPs)
	require.Equal(t, baseLimits().VCPUs, effective.VCPUs, "unset dimension must inherit")
}

// Clearing must delete the record rather than leave an empty one, so "has this
// account been touched?" stays a single existence check.
func TestPutAccountQuotaEmptyClears(t *testing.T) {
	svc, kv := newOverrideService(t)
	const account = "000000000002"

	require.NoError(t, svc.PutAccountQuota(t.Context(), account, Overrides{VCPUs: ptr(32)}, "operator"))
	require.NoError(t, svc.PutAccountQuota(t.Context(), account, Overrides{}, "operator"))

	_, err := kv.Get(t.Context(), account)
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	require.Equal(t, baseLimits(), svc.limitsFor(t.Context(), account))
}

// Clearing an account that never had a record must succeed rather than fail on
// the missing key.
func TestPutAccountQuotaEmptyOnAbsentKey(t *testing.T) {
	svc, _ := newOverrideService(t)
	require.NoError(t, svc.PutAccountQuota(t.Context(), "000000000009", Overrides{}, "operator"))
}

// An explicit zero must survive the store and deny every request, which is the
// difference between a limit of zero and an absent override.
func TestPutAccountQuotaExplicitZero(t *testing.T) {
	svc, _ := newOverrideService(t)
	const account = "000000000002"

	require.NoError(t, svc.PutAccountQuota(t.Context(), account, Overrides{VPCs: ptr(0)}, "operator"))

	require.Equal(t, 0, svc.limitsFor(t.Context(), account).VPCs)
	require.Error(t, exceeds(0, 1, svc.limitsFor(t.Context(), account).VPCs))
}

// A corrupt record is a read failure, not an absent one: it must fall back to
// the configured limits rather than to no limit at all.
func TestLimitsForCorruptRecordFailsClosed(t *testing.T) {
	svc, kv := newOverrideService(t)
	const account = "000000000002"

	_, err := kv.Put(t.Context(), account, []byte("{not json"))
	require.NoError(t, err)

	require.Equal(t, baseLimits(), svc.limitsFor(t.Context(), account),
		"a corrupt override must fall back to the configured limits")

	_, _, err = svc.GetAccountQuota(t.Context(), account)
	require.Error(t, err, "an operator inspecting a quota must see the failure, not the fallback")
}

// A gateway with no override bucket must refuse a write rather than silently
// accept one it cannot store.
func TestPutAccountQuotaWithoutBucket(t *testing.T) {
	svc := New(baseLimits(), nil)
	require.Error(t, svc.PutAccountQuota(t.Context(), "000000000002", Overrides{VCPUs: ptr(32)}, "operator"))
}
