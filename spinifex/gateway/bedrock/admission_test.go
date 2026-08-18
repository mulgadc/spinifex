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
