package gateway_ec2_instance

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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

// --- runInstancesWithClientToken (extracted orchestration) ---

// A completed token replays its reservation and must NOT invoke the launcher.
func TestRunInstancesWithClientToken_ReplaySkipsLaunch(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "rt-1", "h"
	_, owned, err := store.Claim(ctTestAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(ctTestAccount, tok, hash, &ec2.Reservation{ReservationId: aws.String("r-x")}))

	launched := false
	res, err := runInstancesWithClientToken(store, ctTestAccount, tok, hash, func() (ec2.Reservation, error) {
		launched = true
		return ec2.Reservation{}, nil
	})
	require.NoError(t, err)
	assert.False(t, launched, "replay must not launch")
	assert.Equal(t, "r-x", aws.StringValue(res.ReservationId))
}

// The owner launches once and finalizes; a duplicate replays without launching.
func TestRunInstancesWithClientToken_OwnerLaunchesOnceThenReplay(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "rt-2", "h"
	launches := 0
	launch := func() (ec2.Reservation, error) {
		launches++
		return ec2.Reservation{ReservationId: aws.String("r-own")}, nil
	}

	res, err := runInstancesWithClientToken(store, ctTestAccount, tok, hash, launch)
	require.NoError(t, err)
	assert.Equal(t, "r-own", aws.StringValue(res.ReservationId))

	res2, err := runInstancesWithClientToken(store, ctTestAccount, tok, hash, launch)
	require.NoError(t, err)
	assert.Equal(t, "r-own", aws.StringValue(res2.ReservationId))
	assert.Equal(t, 1, launches, "duplicate must replay, not relaunch")
}

// A launch failure aborts the token so a retry re-launches.
func TestRunInstancesWithClientToken_LaunchFailureAborts(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "rt-3", "h"

	_, err := runInstancesWithClientToken(store, ctTestAccount, tok, hash, func() (ec2.Reservation, error) {
		return ec2.Reservation{}, errors.New("no capacity")
	})
	require.Error(t, err)

	relaunched := false
	res, err := runInstancesWithClientToken(store, ctTestAccount, tok, hash, func() (ec2.Reservation, error) {
		relaunched = true
		return ec2.Reservation{ReservationId: aws.String("r-retry")}, nil
	})
	require.NoError(t, err)
	assert.True(t, relaunched, "after abort the retry must launch")
	assert.Equal(t, "r-retry", aws.StringValue(res.ReservationId))
}

// Token reuse with different params maps to the AWS IdempotentParameterMismatch
// error code and never launches.
func TestRunInstancesWithClientToken_ParamMismatchMapsAWSError(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok = "rt-4"
	_, owned, err := store.Claim(ctTestAccount, tok, "hA")
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(ctTestAccount, tok, "hA", &ec2.Reservation{ReservationId: aws.String("r")}))

	launched := false
	_, err = runInstancesWithClientToken(store, ctTestAccount, tok, "hB", func() (ec2.Reservation, error) {
		launched = true
		return ec2.Reservation{}, nil
	})
	require.EqualError(t, err, awserrors.ErrorIdempotentParameterMismatch)
	assert.False(t, launched)
}

// getClientTokenStore binds the process-wide store once and returns the same
// instance on subsequent calls.
func TestGetClientTokenStore_BindsOnce(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	s1, err := getClientTokenStore(nc)
	require.NoError(t, err)
	require.NotNil(t, s1)
	s2, err := getClientTokenStore(nc)
	require.NoError(t, err)
	assert.Same(t, s1, s2, "store binds once")
}

// A duplicate caller polling an in-flight winner that never finishes bails out
// with the wait-timeout sentinel (exercises the Claim poll loop + deadline).
func TestClientToken_InFlightWaitTimesOut(t *testing.T) {
	store := newTestClientTokenStore(t)
	const tok, hash = "wt-1", "h"

	_, owned, err := store.Claim(ctTestAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned, "owner holds the in-flight record and never finalizes")

	origTimeout, origStep := clientTokenWaitTimeout, clientTokenPollStep
	clientTokenWaitTimeout, clientTokenPollStep = 30*time.Millisecond, 10*time.Millisecond
	defer func() { clientTokenWaitTimeout, clientTokenPollStep = origTimeout, origStep }()

	_, _, err = store.Claim(ctTestAccount, tok, hash)
	assert.ErrorIs(t, err, errClientTokenWaitTimeout)
}
