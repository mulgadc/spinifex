package idempotency_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccount = "111122223333"

// payload stands in for whatever a caller replays. A struct rather than a string
// so the JSON round-trip through the record is actually exercised.
type payload struct {
	ID string `json:"id"`
}

func newTestStore[T any](t *testing.T) *idempotency.Store[T] {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := idempotency.OpenStore[T](t.Context(), testutil.NewJetStream(t, nc), "test-tokens", time.Minute)
	require.NoError(t, err)
	return store
}

// A completed token replays its payload to a duplicate caller with matching
// params, which is the whole point of the store.
func TestStore_ReplaysCompletedPayload(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok, hash = "tok-1", "hash-a"

	_, owned, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned, "first caller owns the work")

	require.NoError(t, store.Finalize(t.Context(), testAccount, tok, hash, payload{ID: "r-123"}))

	replay, owned2, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	assert.False(t, owned2, "duplicate caller does not own the work")
	require.NotNil(t, replay)
	assert.Equal(t, "r-123", replay.ID)
}

// The payload type is the caller's: EKS replays a bare cluster name, so a
// non-struct T has to round-trip too.
func TestStore_ReplaysAStringPayload(t *testing.T) {
	t.Parallel()
	store := newTestStore[string](t)
	const tok, hash = "tok-str", "hash-a"

	_, owned, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), testAccount, tok, hash, "my-cluster"))

	replay, _, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.NotNil(t, replay)
	assert.Equal(t, "my-cluster", *replay)
}

// Reusing a token with different params is the mismatch case, which each caller
// maps onto IdempotentParameterMismatch.
func TestStore_ParamMismatchRejected(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok = "tok-2"

	_, owned, err := store.Claim(t.Context(), testAccount, tok, "hash-a")
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), testAccount, tok, "hash-a", payload{ID: "r-1"}))

	_, _, err = store.Claim(t.Context(), testAccount, tok, "hash-DIFFERENT")
	assert.ErrorIs(t, err, idempotency.ErrParamMismatch)
}

// A failed attempt aborts the token so a later retry re-runs the work instead of
// replaying a result that was never produced.
func TestStore_AbortAllowsRetry(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok, hash = "tok-3", "hash-a"

	_, owned, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)

	store.Abort(t.Context(), testAccount, tok)

	_, owned2, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	assert.True(t, owned2, "after abort the token is free to re-own")
}

// Concurrent callers with the same token must yield exactly one owner and the
// rest replay it. This is the property that stops a double launch under -race.
func TestStore_ConcurrentSingleOwner(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok, hash = "tok-4", "hash-a"
	done := payload{ID: "r-only"}

	const n = 4
	var owners int32
	var wg sync.WaitGroup
	errs := make([]error, n)
	replays := make([]*payload, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replay, owned, err := store.Claim(t.Context(), testAccount, tok, hash)
			if err != nil {
				errs[i] = err
				return
			}
			if owned {
				atomic.AddInt32(&owners, 1)
				errs[i] = store.Finalize(t.Context(), testAccount, tok, hash, done)
				replays[i] = &done
				return
			}
			replays[i] = replay
		}(i)
	}
	wg.Wait()

	for _, e := range errs {
		require.NoError(t, e)
	}
	assert.Equal(t, int32(1), owners, "exactly one caller runs the work")
	for _, r := range replays {
		require.NotNil(t, r, "every caller ends with the single result")
		assert.Equal(t, "r-only", r.ID)
	}
}

// The same token under two accounts is two separate pieces of work, so one
// account's result must never be replayed to another.
func TestStore_TokensAreScopedPerAccount(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok, hash = "shared-token", "h"

	_, owned, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), testAccount, tok, hash, payload{ID: "r-first"}))

	replay, owned2, err := store.Claim(t.Context(), "999988887777", tok, hash)
	require.NoError(t, err)
	assert.True(t, owned2, "a second account owns its own work under the same token")
	assert.Nil(t, replay, "one account must never replay another's result")
}

// ParamHash reflects the request it is given, which is what makes a reused token
// with changed parameters detectable.
func TestParamHash_DistinguishesRequests(t *testing.T) {
	t.Parallel()
	first := idempotency.ParamHash(payload{ID: "a"})
	again := idempotency.ParamHash(payload{ID: "a"})
	changed := idempotency.ParamHash(payload{ID: "b"})

	assert.Equal(t, first, again, "the same request must always hash the same")
	assert.NotEqual(t, first, changed, "a changed request must change the hash")
}

// Actions share one bucket, so one action's token must never be seen — or
// decoded as the wrong type — by another.
func TestStore_NamespacesDoNotCollide(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := idempotency.OpenBucket(t.Context(), testutil.NewJetStream(t, nc), "ns-tokens", time.Minute)
	require.NoError(t, err)

	volumes := idempotency.NewStore[payload](kv, "CreateVolume")
	gateways := idempotency.NewStore[payload](kv, "CreateNatGateway")
	const tok, hash = "shared-token", "h"

	_, owned, err := volumes.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, volumes.Finalize(t.Context(), testAccount, tok, hash, payload{ID: "vol-1"}))

	replay, owned, err := gateways.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	assert.True(t, owned, "a different action owns its own work under the same token")
	assert.Nil(t, replay, "one action must never replay another's result")
}

// The stores that predate namespacing keep bare account.token keys: re-keying
// live records would make a retry re-run the work instead of replaying it.
func TestStore_EmptyNamespaceKeepsTheBareKey(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv, err := idempotency.OpenBucket(t.Context(), js, "bare-tokens", time.Minute)
	require.NoError(t, err)

	store := idempotency.NewStore[payload](kv, "")
	const tok, hash = "bare-1", "h"
	_, owned, err := store.Claim(t.Context(), testAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)

	_, err = kv.Get(t.Context(), idempotency.Key(testAccount, tok))
	assert.NoError(t, err, "the record must live at the unprefixed key")
}
