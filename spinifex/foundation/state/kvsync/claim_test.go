package kvsync_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaim_SecondHolderIsRefusedAndSameHolderAdoptsWithoutAWrite(t *testing.T) {
	s := stores(t, 3)
	a := kvsync.NewClaim(s[0], "claim.x", "node-a")
	b := kvsync.NewClaim(s[1], "claim.x", "node-b")
	restarted := kvsync.NewClaim(s[2], "claim.x", "node-a")

	held, err := a.TryLock(t.Context())
	require.NoError(t, err)

	_, err = b.TryLock(t.Context())
	he, ok := asHeld(err)
	require.True(t, ok, "want *HeldError, got %v", err)
	assert.Equal(t, held, he.Current, "the refusal must name the holder and its generation")

	before := revision(t, s[0], "claim.x")
	adopted, err := restarted.TryLock(t.Context())
	require.NoError(t, err)
	assert.Equal(t, held, adopted, "adoption keeps the generation")
	assert.Equal(t, before, revision(t, s[0], "claim.x"), "adoption must not write")
}

func TestClaim_TakeOverNeedsTheCurrentToken(t *testing.T) {
	s := stores(t, 3)
	a := kvsync.NewClaim(s[0], "claim.x", "node-a")
	b := kvsync.NewClaim(s[1], "claim.x", "node-b")
	c := kvsync.NewClaim(s[2], "claim.x", "node-c")

	first, err := a.TryLock(t.Context())
	require.NoError(t, err)

	second, err := b.TakeOver(t.Context(), first)
	require.NoError(t, err)
	assert.Equal(t, "node-b", second.Holder)
	assert.Greater(t, second.Generation, first.Generation)

	// c judged node-a dead, but node-b has moved the key on since.
	_, err = c.TakeOver(t.Context(), first)
	he, ok := asHeld(err)
	require.True(t, ok, "want *HeldError, got %v", err)
	assert.Equal(t, second, he.Current)
}

func TestClaim_UnlockAfterTakeOverIsLostAndLeavesTheNewHolder(t *testing.T) {
	s := stores(t, 2)
	a := kvsync.NewClaim(s[0], "claim.x", "node-a")
	b := kvsync.NewClaim(s[1], "claim.x", "node-b")

	first, err := a.TryLock(t.Context())
	require.NoError(t, err)
	second, err := b.TakeOver(t.Context(), first)
	require.NoError(t, err)

	require.ErrorIs(t, a.Unlock(t.Context(), first), kvsync.ErrLost)

	cur, ok, err := a.Holder(t.Context())
	require.NoError(t, err)
	require.True(t, ok, "the stale unlock deleted the new holder's key")
	assert.Equal(t, second, cur)
}

func TestClaim_RelockAfterUnlockGetsAHigherGeneration(t *testing.T) {
	s := stores(t, 1)
	a := kvsync.NewClaim(s[0], "claim.x", "node-a")

	first, err := a.TryLock(t.Context())
	require.NoError(t, err)
	require.NoError(t, a.Unlock(t.Context(), first))

	_, ok, err := a.Holder(t.Context())
	require.NoError(t, err)
	require.False(t, ok, "unlock must free the key")

	second, err := a.TryLock(t.Context())
	require.NoError(t, err)
	assert.Greater(t, second.Generation, first.Generation, "a generation must outrank every earlier one, across a delete")
}

func TestClaimGroup_HandsOutKeysUnderItsPrefix(t *testing.T) {
	s := stores(t, 2)
	ga := kvsync.NewClaimGroup(s[0], "uefi.", "node-a")
	gb := kvsync.NewClaimGroup(s[1], "uefi.", "node-b")

	_, err := ga.Get("i-1").TryLock(t.Context())
	require.NoError(t, err)
	_, err = gb.Get("i-1").TryLock(t.Context())
	_, ok := asHeld(err)
	assert.True(t, ok, "the same name in two groups is the same key")
	_, err = gb.Get("i-2").TryLock(t.Context())
	assert.NoError(t, err, "a different name is a different key")
	assert.NotZero(t, revision(t, s[0], "uefi.i-1"))
}

// A peer that moves the key between the take-over's read and its write wins,
// and the refusal names that peer rather than the holder judged dead.
func TestClaim_TakeOverThatLosesTheRaceNamesTheWinner(t *testing.T) {
	s := stores(t, 3)
	a := kvsync.NewClaim(s[0], "claim.x", "node-a")
	b := kvsync.NewClaim(s[1], "claim.x", "node-b")

	first, err := a.TryLock(t.Context())
	require.NoError(t, err)
	var winner kvsync.Token
	racing := &racingStore{KVStore: s[2], race: func() {
		winner, err = b.TakeOver(t.Context(), first)
		require.NoError(t, err)
	}}
	c := kvsync.NewClaim(racing, "claim.x", "node-c")

	_, err = c.TakeOver(t.Context(), first)
	he, ok := asHeld(err)
	require.True(t, ok, "want *HeldError, got %v", err)
	assert.Equal(t, winner, he.Current)
}

func TestClaim_UnlockOfAnAbsentKeyIsLost(t *testing.T) {
	s := stores(t, 1)
	a := kvsync.NewClaim(s[0], "claim.x", "node-a")

	first, err := a.TryLock(t.Context())
	require.NoError(t, err)
	require.NoError(t, a.Unlock(t.Context(), first))
	require.ErrorIs(t, a.Unlock(t.Context(), first), kvsync.ErrLost, "a key already gone is not this holder's to free")
}
