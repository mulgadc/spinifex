package gateway_ec2_instance

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ctTestAccount = "111122223333"

func newTestClientTokenStore(t *testing.T) *ClientTokenStore {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store, err := newClientTokenStore(js)
	require.NoError(t, err)
	return store
}

// A completed token replays the stored reservation for a duplicate caller with
// matching params.
func TestClientToken_ReplaysCompletedReservation(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "tok-1", "hash-a"

	_, owned, err := store.Claim(ctTestAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned, "first caller owns the launch")

	res := &ec2.Reservation{ReservationId: aws.String("r-123")}
	require.NoError(t, store.Finalize(ctTestAccount, tok, hash, res))

	replay, owned2, err := store.Claim(ctTestAccount, tok, hash)
	require.NoError(t, err)
	assert.False(t, owned2, "duplicate caller does not own the launch")
	require.NotNil(t, replay)
	assert.Equal(t, "r-123", aws.StringValue(replay.ReservationId))
}

// Reusing a token with different params is IdempotentParameterMismatch.
func TestClientToken_ParamMismatchRejected(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok = "tok-2"

	_, owned, err := store.Claim(ctTestAccount, tok, "hash-a")
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(ctTestAccount, tok, "hash-a", &ec2.Reservation{ReservationId: aws.String("r-1")}))

	_, _, err = store.Claim(ctTestAccount, tok, "hash-DIFFERENT")
	assert.ErrorIs(t, err, errIdempotentParamMismatch)
}

// A failed launch aborts the token so a later retry with the same token
// re-launches instead of replaying a non-existent reservation.
func TestClientToken_AbortAllowsRelaunch(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "tok-3", "hash-a"

	_, owned, err := store.Claim(ctTestAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)

	store.Abort(ctTestAccount, tok)

	_, owned2, err := store.Claim(ctTestAccount, tok, hash)
	require.NoError(t, err)
	assert.True(t, owned2, "after abort the token is free to re-own")
}

// Concurrent callers with the same token must yield exactly one owner; the
// others replay the owner's reservation. Proves single-launch under -race.
func TestClientToken_ConcurrentSingleOwner(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "tok-4", "hash-a"
	res := &ec2.Reservation{ReservationId: aws.String("r-only")}

	const n = 4
	var owners int32
	var wg sync.WaitGroup
	errs := make([]error, n)
	replays := make([]*ec2.Reservation, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replay, owned, err := store.Claim(ctTestAccount, tok, hash)
			if err != nil {
				errs[i] = err
				return
			}
			if owned {
				atomic.AddInt32(&owners, 1)
				// Simulate the launch then publish the reservation.
				errs[i] = store.Finalize(ctTestAccount, tok, hash, res)
				replays[i] = res
				return
			}
			replays[i] = replay
		}(i)
	}
	wg.Wait()

	for _, e := range errs {
		require.NoError(t, e)
	}
	assert.Equal(t, int32(1), owners, "exactly one caller launches")
	for _, r := range replays {
		require.NotNil(t, r, "every caller ends with the single reservation")
		assert.Equal(t, "r-only", aws.StringValue(r.ReservationId))
	}
}

// clientTokenParamHash ignores ClientToken (same params, different token →
// same hash) but reflects a real parameter change.
func TestClientTokenParamHash_IgnoresTokenReflectsParams(t *testing.T) {
	base := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-123"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		ClientToken:  aws.String("tok-A"),
	}
	sameParamsDiffToken := *base
	sameParamsDiffToken.ClientToken = aws.String("tok-B")
	diffParams := *base
	diffParams.InstanceType = aws.String("t3.large")

	assert.Equal(t, clientTokenParamHash(base), clientTokenParamHash(&sameParamsDiffToken),
		"token must not affect the param hash")
	assert.NotEqual(t, clientTokenParamHash(base), clientTokenParamHash(&diffParams),
		"a real param change must change the hash")
}
