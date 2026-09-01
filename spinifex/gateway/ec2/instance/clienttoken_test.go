package gateway_ec2_instance

import (
	"errors"
	"testing"

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
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := newClientTokenStore(t.Context(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	return store
}

// The claim/replay/abort mechanics live in spinifex/idempotency and are tested
// there. What is EC2's own is the param hash, the launch orchestration around
// the store, and the AWS error a mismatch maps onto.

// clientTokenParamHash ignores ClientToken (same params, different token →
// same hash) but reflects a real parameter change.
func TestClientTokenParamHash_IgnoresTokenReflectsParams(t *testing.T) {
	t.Parallel()
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

// The hash is taken from a copy, so hashing must not clear the caller's token.
func TestClientTokenParamHash_LeavesTheInputAlone(t *testing.T) {
	t.Parallel()
	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-123"), ClientToken: aws.String("tok-A")}

	clientTokenParamHash(input)

	assert.Equal(t, "tok-A", aws.StringValue(input.ClientToken),
		"hashing must not strip the token the launch still needs")
}

// A completed token replays its reservation and must NOT invoke the launcher.
func TestRunInstancesWithClientToken_ReplaySkipsLaunch(t *testing.T) {
	t.Parallel()
	store := newTestClientTokenStore(t)
	const tok, hash = "rt-1", "h"
	_, owned, err := store.Claim(t.Context(), ctTestAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), ctTestAccount, tok, hash, ec2.Reservation{ReservationId: aws.String("r-x")}))

	launched := false
	res, err := runInstancesWithClientToken(t.Context(), store, ctTestAccount, tok, hash, func() (ec2.Reservation, error) {
		launched = true
		return ec2.Reservation{}, nil
	})
	require.NoError(t, err)
	assert.False(t, launched, "replay must not launch")
	assert.Equal(t, "r-x", aws.StringValue(res.ReservationId))
}

// The owner launches once and finalizes; a duplicate replays without launching.
func TestRunInstancesWithClientToken_OwnerLaunchesOnceThenReplay(t *testing.T) {
	t.Parallel()
	store := newTestClientTokenStore(t)
	const tok, hash = "rt-2", "h"
	launches := 0
	launch := func() (ec2.Reservation, error) {
		launches++
		return ec2.Reservation{ReservationId: aws.String("r-own")}, nil
	}

	res, err := runInstancesWithClientToken(t.Context(), store, ctTestAccount, tok, hash, launch)
	require.NoError(t, err)
	assert.Equal(t, "r-own", aws.StringValue(res.ReservationId))

	res2, err := runInstancesWithClientToken(t.Context(), store, ctTestAccount, tok, hash, launch)
	require.NoError(t, err)
	assert.Equal(t, "r-own", aws.StringValue(res2.ReservationId))
	assert.Equal(t, 1, launches, "duplicate must replay, not relaunch")
}

// A launch failure aborts the token so a retry re-launches.
func TestRunInstancesWithClientToken_LaunchFailureAborts(t *testing.T) {
	t.Parallel()
	store := newTestClientTokenStore(t)
	const tok, hash = "rt-3", "h"

	_, err := runInstancesWithClientToken(t.Context(), store, ctTestAccount, tok, hash, func() (ec2.Reservation, error) {
		return ec2.Reservation{}, errors.New("no capacity")
	})
	require.Error(t, err)

	relaunched := false
	res, err := runInstancesWithClientToken(t.Context(), store, ctTestAccount, tok, hash, func() (ec2.Reservation, error) {
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
	t.Parallel()
	store := newTestClientTokenStore(t)
	const tok = "rt-4"
	_, owned, err := store.Claim(t.Context(), ctTestAccount, tok, "hA")
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), ctTestAccount, tok, "hA", ec2.Reservation{ReservationId: aws.String("r")}))

	launched := false
	_, err = runInstancesWithClientToken(t.Context(), store, ctTestAccount, tok, "hB", func() (ec2.Reservation, error) {
		launched = true
		return ec2.Reservation{}, nil
	})
	require.EqualError(t, err, awserrors.ErrorIdempotentParameterMismatch)
	assert.False(t, launched)
}

// getClientTokenStore binds the process-wide store once and returns the same
// instance on subsequent calls.
func TestGetClientTokenStore_BindsOnce(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	s1, err := getClientTokenStore(t.Context(), nc)
	require.NoError(t, err)
	require.NotNil(t, s1)
	s2, err := getClientTokenStore(t.Context(), nc)
	require.NoError(t, err)
	assert.Same(t, s1, s2, "store binds once")
}
