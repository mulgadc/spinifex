package handlers_eks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcilerRegistry_SpawnAndStop(t *testing.T) {
	reg := NewReconcilerRegistry()

	var (
		ran      atomic.Bool
		released atomic.Bool
	)
	fn := func(ctx context.Context, accountID, clusterName string) (func(), error) {
		ran.Store(true)
		go func() { <-ctx.Done() }()
		return func() { released.Store(true) }, nil
	}

	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	assert.True(t, reg.Has("111122223333", "alpha"))
	assert.True(t, ran.Load())

	reg.Stop("111122223333", "alpha")
	assert.False(t, reg.Has("111122223333", "alpha"))
	require.Eventually(t, released.Load, 500*time.Millisecond, 5*time.Millisecond)
}

func TestReconcilerRegistry_SpawnIdempotent(t *testing.T) {
	reg := NewReconcilerRegistry()
	var spawnCalls atomic.Int32
	fn := func(ctx context.Context, _, _ string) (func(), error) {
		spawnCalls.Add(1)
		go func() { <-ctx.Done() }()
		return func() {}, nil
	}

	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))

	assert.EqualValues(t, 1, spawnCalls.Load(), "second Spawn must not re-invoke fn")
	reg.StopAll()
}

func TestReconcilerRegistry_SpawnFnErrorRemovesEntry(t *testing.T) {
	reg := NewReconcilerRegistry()
	fn := func(_ context.Context, _, _ string) (func(), error) {
		return nil, errors.New("acquire lease failed")
	}

	err := reg.Spawn(t.Context(), "111122223333", "alpha", fn)
	require.Error(t, err)
	assert.False(t, reg.Has("111122223333", "alpha"), "entry must not linger after spawn failure")
}

func TestReconcilerRegistry_LeaseHeldDropsEntryAndRetries(t *testing.T) {
	reg := NewReconcilerRegistry()
	var spawnCalls atomic.Int32
	leaseHeld := atomic.Bool{}
	leaseHeld.Store(true)
	fn := func(ctx context.Context, _, _ string) (func(), error) {
		spawnCalls.Add(1)
		if leaseHeld.Load() {
			return nil, ErrLeaseHeld
		}
		go func() { <-ctx.Done() }()
		return func() {}, nil
	}

	// Lease held elsewhere: Spawn is benign (nil), records no phantom holder.
	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	assert.False(t, reg.Has("111122223333", "alpha"), "lease-held must not leave a holder")

	// Holder's TTL lapses; a later Spawn re-invokes fn and now takes over —
	// no daemon restart needed.
	leaseHeld.Store(false)
	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	assert.True(t, reg.Has("111122223333", "alpha"))
	assert.EqualValues(t, 2, spawnCalls.Load(), "second Spawn must re-attempt after lease-held")
	reg.StopAll()
}

func TestReconcilerRegistry_StopAllCancelsEvery(t *testing.T) {
	reg := NewReconcilerRegistry()
	var released atomic.Int32
	fn := func(ctx context.Context, _, _ string) (func(), error) {
		go func() { <-ctx.Done() }()
		return func() { released.Add(1) }, nil
	}

	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "beta", fn))
	require.NoError(t, reg.Spawn(t.Context(), "444455556666", "gamma", fn))

	reg.StopAll()
	require.Eventually(t, func() bool { return released.Load() == 3 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.False(t, reg.Has("111122223333", "alpha"))
	assert.False(t, reg.Has("111122223333", "beta"))
	assert.False(t, reg.Has("444455556666", "gamma"))
}

func TestReconcilerRegistry_StopUnknownKeyNoop(t *testing.T) {
	reg := NewReconcilerRegistry()
	reg.Stop("111122223333", "ghost")
	assert.False(t, reg.Has("111122223333", "ghost"))
}

func TestReconcilerRegistry_SpawnRejectsBadArgs(t *testing.T) {
	reg := NewReconcilerRegistry()
	fn := func(_ context.Context, _, _ string) (func(), error) { return func() {}, nil }

	require.Error(t, reg.Spawn(t.Context(), "", "alpha", fn))
	require.Error(t, reg.Spawn(t.Context(), "111122223333", "", fn))
	require.Error(t, reg.Spawn(t.Context(), "111122223333", "alpha", nil))
}
