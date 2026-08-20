// slotReleasingSource Close bookkeeping directly, which cannot move to an
// external test package.
//
//test:in-package — drives the unexported concurrencyLimiter and
package gateway_bedrock

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// admissionFakeConverseStreamSource is a minimal converseStreamSource double
// for exercising slotReleasingSource's Close-call bookkeeping directly,
// independent of the pumpConverseStream-oriented fakeConverseStreamSource in
// converse_stream_test.go.
type admissionFakeConverseStreamSource struct {
	closeCalls int
	closeErr   error
}

var _ converseStreamSource = (*admissionFakeConverseStreamSource)(nil)

func (f *admissionFakeConverseStreamSource) Next(_ context.Context) (ConverseStreamEvent, bool, error) {
	return ConverseStreamEvent{}, false, nil
}

func (f *admissionFakeConverseStreamSource) Close() error {
	f.closeCalls++
	return f.closeErr
}

// admissionFakeInvokeStreamSource is slotReleasingInvokeSource's counterpart
// to admissionFakeConverseStreamSource, independent of the
// pumpInvokeStream-oriented fakeInvokeStreamSource in invoke_stream_test.go.
type admissionFakeInvokeStreamSource struct {
	closeCalls int
	closeErr   error
}

var _ invokeStreamSource = (*admissionFakeInvokeStreamSource)(nil)

func (f *admissionFakeInvokeStreamSource) Next(_ context.Context) ([]byte, bool, error) {
	return nil, false, nil
}

func (f *admissionFakeInvokeStreamSource) Close() error {
	f.closeCalls++
	return f.closeErr
}

func TestConcurrencyLimiter_AcquireAtCapacityRejects(t *testing.T) {
	l := newConcurrencyLimiter()

	release1, ok := l.Acquire("k", 1)
	require.True(t, ok)

	_, ok = l.Acquire("k", 1)
	assert.False(t, ok, "a second concurrent acquire at capacity 1 must be rejected")

	release1()

	release2, ok := l.Acquire("k", 1)
	require.True(t, ok, "after release, the next acquire must succeed")
	release2()
}

func TestConcurrencyLimiter_ReleaseIsIdempotent(t *testing.T) {
	l := newConcurrencyLimiter()

	release, ok := l.Acquire("k", 1)
	require.True(t, ok)

	release()
	release()
	release()

	// A double/triple release must not have driven the counter negative and
	// admitted more than capacity as a result.
	r2, ok := l.Acquire("k", 1)
	require.True(t, ok)
	_, ok = l.Acquire("k", 1)
	assert.False(t, ok, "capacity must still be exactly 1 after the redundant releases")
	r2()
}

func TestConcurrencyLimiter_DifferentKeysAreIndependent(t *testing.T) {
	l := newConcurrencyLimiter()

	releaseA, ok := l.Acquire("a", 1)
	require.True(t, ok)
	defer releaseA()

	_, ok = l.Acquire("b", 1)
	assert.True(t, ok, "a different key must have its own independent capacity")
}

// TestConcurrencyLimiter_CapacityMultipliesWithConcurrentAcquires table-drives
// capacity values shaped like catalog MaxConcurrency x ModelUnits products,
// asserting exactly `capacity` concurrent acquires are ever admitted.
func TestConcurrencyLimiter_CapacityMultipliesWithConcurrentAcquires(t *testing.T) {
	tests := []struct {
		name           string
		maxConcurrency int
		modelUnits     int
		concurrent     int
	}{
		{"1B default x ON_DEMAND (units=1)", 256, 1, 300},
		{"3B x ON_DEMAND (units=1)", 8, 1, 20},
		{"3B x ModelUnits=2", 8, 2, 30},
		{"3B x ModelUnits=4", 8, 4, 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newConcurrencyLimiter()
			capacity := tt.maxConcurrency * tt.modelUnits

			admitted := 0
			for i := 0; i < tt.concurrent; i++ {
				if _, ok := l.Acquire("k", capacity); ok {
					admitted++
				}
			}
			assert.Equal(t, capacity, admitted)
		})
	}
}

func TestSlotReleasingSource_ClosesReleaseExactlyOnceAndIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	releases := 0
	release := func() {
		mu.Lock()
		defer mu.Unlock()
		releases++
	}

	inner := &admissionFakeConverseStreamSource{}
	src := newSlotReleasingSource(inner, release)

	require.NoError(t, src.Close())
	require.NoError(t, src.Close())
	require.NoError(t, src.Close())

	assert.Equal(t, 1, releases, "the admission slot must release exactly once")
	assert.Equal(t, 3, inner.closeCalls, "every Close call must still reach the wrapped source")
}

func TestSlotReleasingSource_ReleasesEvenWhenInnerCloseErrors(t *testing.T) {
	released := false
	release := func() { released = true }

	inner := &admissionFakeConverseStreamSource{closeErr: errors.New("upstream close failed")}
	src := newSlotReleasingSource(inner, release)

	err := src.Close()
	assert.Error(t, err)
	assert.True(t, released, "the slot must release even when the inner Close errors")
}

func TestSlotReleasingInvokeSource_ClosesReleaseExactlyOnceAndIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	releases := 0
	release := func() {
		mu.Lock()
		defer mu.Unlock()
		releases++
	}

	inner := &admissionFakeInvokeStreamSource{}
	src := newSlotReleasingInvokeSource(inner, release)

	require.NoError(t, src.Close())
	require.NoError(t, src.Close())

	assert.Equal(t, 1, releases)
	assert.Equal(t, 2, inner.closeCalls)
}

// TestAdmitSelfHost_OnDemandAdmitsThenThrottles drives the ON_DEMAND path
// (empty servingAccountID, units default to 1) end to end: the first request
// is admitted, the second at capacity returns a ThrottlingException, and
// capacity frees up once the in-flight release runs.
func TestAdmitSelfHost_OnDemandAdmitsThenThrottles(t *testing.T) {
	ctx := context.Background()
	entry := catalogEntry{MaxConcurrency: 1}
	const model = "admit-ondemand-throttle-model"

	release, err := admitSelfHost(ctx, nil, "", model, entry)
	require.NoError(t, err)
	require.NotNil(t, release)

	_, err = admitSelfHost(ctx, nil, "", model, entry)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error(),
		"a self-host request at capacity must return a ThrottlingException")

	release()
	release2, err := admitSelfHost(ctx, nil, "", model, entry)
	require.NoError(t, err, "capacity must free up after the in-flight request releases")
	release2()
}

// TestAdmitSelfHost_CommittedUnitsErrorPropagates asserts a store failure while
// resolving committed ModelUnits surfaces as that error, never masquerading as
// a throttle or admitting the request.
func TestAdmitSelfHost_CommittedUnitsErrorPropagates(t *testing.T) {
	store := NewProvisionedStore(nil, 1, ptTestRegion, newStubEndpointProvisioner())

	release, err := admitSelfHost(context.Background(), store, ptCallerAccount, selfHostTestModel,
		catalogEntry{MaxConcurrency: 1})
	require.Error(t, err)
	assert.Nil(t, release)
	assert.NotEqual(t, awserrors.ErrorThrottlingException, err.Error(),
		"a store failure must not masquerade as a throttle")
}

// TestAdmitSelfHost_ClampsZeroCommittedUnits covers a serving account with no
// commitment for the model: committed units read zero and capacity clamps to
// MaxConcurrency x 1 rather than collapsing to zero and rejecting everything.
func TestAdmitSelfHost_ClampsZeroCommittedUnits(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()
	entry := catalogEntry{MaxConcurrency: 1}

	release, err := admitSelfHost(ctx, store, ptCallerAccount, selfHostTestModel, entry)
	require.NoError(t, err)
	require.NotNil(t, release)
	defer release()

	_, err = admitSelfHost(ctx, store, ptCallerAccount, selfHostTestModel, entry)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error(),
		"clamped capacity is exactly 1, so a second concurrent request throttles")
}

// TestAdmitSelfHost_StacksCommittedModelUnits proves two commitments on the
// same account+model stack their ModelUnits into one shared capacity: exactly
// MaxConcurrency x (2+3) concurrent requests are admitted before throttling.
func TestAdmitSelfHost_StacksCommittedModelUnits(t *testing.T) {
	stub := newStubEndpointProvisioner()
	store := newProvisionedTestStore(t, stub)
	ctx := context.Background()

	_, err := CreateProvisionedModelThroughput(ctx, ptCallerAccount, store, createInput(selfHostTestModel, "pt-a", 2))
	require.NoError(t, err)
	_, err = CreateProvisionedModelThroughput(ctx, ptCallerAccount, store, createInput(selfHostTestModel, "pt-b", 3))
	require.NoError(t, err)

	entry := catalogEntry{MaxConcurrency: 1}
	const wantCapacity = 5

	releases := make([]func(), 0, wantCapacity)
	for i := range wantCapacity {
		release, err := admitSelfHost(ctx, store, ptCallerAccount, selfHostTestModel, entry)
		require.NoError(t, err, "acquire %d must be admitted within stacked capacity", i)
		releases = append(releases, release)
	}
	_, err = admitSelfHost(ctx, store, ptCallerAccount, selfHostTestModel, entry)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error(),
		"the request past stacked capacity must throttle")

	for _, release := range releases {
		release()
	}
}

// TestCommittedModelUnits_EmptyAccountReturnsZero covers the shared ON_DEMAND
// short-circuit: an empty serving account has no commitment to read and must
// not touch the store, so a nil store is safe.
func TestCommittedModelUnits_EmptyAccountReturnsZero(t *testing.T) {
	units, err := committedModelUnits(context.Background(), nil, "", selfHostTestModel)
	require.NoError(t, err)
	assert.Equal(t, int64(0), units)
}

// TestCommittedModelUnits_BucketErrorPropagates asserts an unusable store
// surfaces its error rather than silently reading zero committed units.
func TestCommittedModelUnits_BucketErrorPropagates(t *testing.T) {
	store := NewProvisionedStore(nil, 1, ptTestRegion, newStubEndpointProvisioner())
	_, err := committedModelUnits(context.Background(), store, ptCallerAccount, selfHostTestModel)
	require.Error(t, err)
}

// TestCommittedModelUnits_NoCommitmentsReturnsZero covers the empty-bucket path:
// a store with no commitments at all reads zero without erroring.
func TestCommittedModelUnits_NoCommitmentsReturnsZero(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	units, err := committedModelUnits(context.Background(), store, ptCallerAccount, selfHostTestModel)
	require.NoError(t, err)
	assert.Equal(t, int64(0), units)
}

// TestCommittedModelUnits_ScopesToAccountAndModel proves the sum stacks only a
// caller's own commitments for the requested model, skipping a different model
// and a different account, and reads zero for a model with no commitment.
func TestCommittedModelUnits_ScopesToAccountAndModel(t *testing.T) {
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	ctx := context.Background()

	mustCreate := func(account, model, name string, units int64) {
		t.Helper()
		_, err := CreateProvisionedModelThroughput(ctx, account, store, createInput(model, name, units))
		require.NoError(t, err)
	}
	mustCreate(ptCallerAccount, selfHostTestModel, "caller-a", 2)
	mustCreate(ptCallerAccount, selfHostTestModel, "caller-b", 3)
	mustCreate(ptCallerAccount, selfHostTestModel3B, "other-model", 4)
	mustCreate(ptOtherCaller, selfHostTestModel, "other-account", 9)

	units, err := committedModelUnits(ctx, store, ptCallerAccount, selfHostTestModel)
	require.NoError(t, err)
	assert.Equal(t, int64(5), units, "only the caller's own commitments for this model stack")

	unknown, err := committedModelUnits(ctx, store, ptCallerAccount, "meta.does-not-exist-v1:0")
	require.NoError(t, err)
	assert.Equal(t, int64(0), unknown, "a model with no commitment reads zero")
}

func TestAdmissionKey_ScopesByAccountAndModel(t *testing.T) {
	assert.NotEqual(t, admissionKey("a", "m"), admissionKey("b", "m"), "different accounts must not share a key")
	assert.NotEqual(t, admissionKey("a", "m1"), admissionKey("a", "m2"), "different models must not share a key")

	l := newConcurrencyLimiter()
	release, ok := l.Acquire(admissionKey("a", "m"), 1)
	require.True(t, ok)
	defer release()

	_, ok = l.Acquire(admissionKey("a", "m"), 1)
	assert.False(t, ok, "the same (account, model) pair must produce the same key and share capacity")
}
