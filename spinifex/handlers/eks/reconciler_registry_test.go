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
	fn := func(ctx context.Context, accountID, clusterName string) (func(), <-chan struct{}, error) {
		ran.Store(true)
		go func() { <-ctx.Done() }()
		return func() { released.Store(true) }, nil, nil
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
	fn := func(ctx context.Context, _, _ string) (func(), <-chan struct{}, error) {
		spawnCalls.Add(1)
		go func() { <-ctx.Done() }()
		return func() {}, nil, nil
	}

	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))

	assert.EqualValues(t, 1, spawnCalls.Load(), "second Spawn must not re-invoke fn")
	reg.StopAll()
}

func TestReconcilerRegistry_SpawnFnErrorRemovesEntry(t *testing.T) {
	reg := NewReconcilerRegistry()
	fn := func(_ context.Context, _, _ string) (func(), <-chan struct{}, error) {
		return nil, nil, errors.New("acquire lease failed")
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
	fn := func(ctx context.Context, _, _ string) (func(), <-chan struct{}, error) {
		spawnCalls.Add(1)
		if leaseHeld.Load() {
			return nil, nil, ErrLeaseHeld
		}
		go func() { <-ctx.Done() }()
		return func() {}, nil, nil
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

// TestReconcilerRegistry_SelfExitDropsEntryAndRespawns covers a reconciler that
// exits on its own — a terminal cluster state or a lost lease — with no Stop()
// to cancel its ctx. The registry must drop the entry (and release the lease) so
// a later same-name create spawns a fresh reconciler instead of silently
// no-opping on a leaked holder.
func TestReconcilerRegistry_SelfExitDropsEntryAndRespawns(t *testing.T) {
	reg := NewReconcilerRegistry()
	var (
		spawnCalls atomic.Int32
		released   atomic.Int32
	)
	// finished models RunClusterReconciler's Run goroutine returning on its own;
	// closing it is the only signal (ctx is never cancelled here).
	finished := make(chan struct{})
	fn := func(_ context.Context, _, _ string) (func(), <-chan struct{}, error) {
		spawnCalls.Add(1)
		return func() { released.Add(1) }, finished, nil
	}

	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn))
	assert.True(t, reg.Has("111122223333", "alpha"))

	close(finished) // reconciler self-exits
	require.Eventually(t, func() bool { return !reg.Has("111122223333", "alpha") },
		500*time.Millisecond, 5*time.Millisecond, "self-exit must drop the registry entry")
	require.Eventually(t, func() bool { return released.Load() == 1 },
		500*time.Millisecond, 5*time.Millisecond, "self-exit must release the lease")

	// A later create under the same name must re-invoke fn, not no-op.
	finished2 := make(chan struct{})
	fn2 := func(ctx context.Context, _, _ string) (func(), <-chan struct{}, error) {
		spawnCalls.Add(1)
		go func() { <-ctx.Done() }()
		return func() {}, finished2, nil
	}
	require.NoError(t, reg.Spawn(t.Context(), "111122223333", "alpha", fn2))
	assert.True(t, reg.Has("111122223333", "alpha"))
	assert.EqualValues(t, 2, spawnCalls.Load(), "same-name create after self-exit must re-invoke fn")
	reg.StopAll()
}

func TestReconcilerRegistry_StopAllCancelsEvery(t *testing.T) {
	reg := NewReconcilerRegistry()
	var released atomic.Int32
	fn := func(ctx context.Context, _, _ string) (func(), <-chan struct{}, error) {
		go func() { <-ctx.Done() }()
		return func() { released.Add(1) }, nil, nil
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
	fn := func(_ context.Context, _, _ string) (func(), <-chan struct{}, error) { return func() {}, nil, nil }

	require.Error(t, reg.Spawn(t.Context(), "", "alpha", fn))
	require.Error(t, reg.Spawn(t.Context(), "111122223333", "", fn))
	require.Error(t, reg.Spawn(t.Context(), "111122223333", "alpha", nil))
}
