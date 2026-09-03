package kvstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenWithRetry_SucceedsFirstAttempt(t *testing.T) {
	calls := 0
	got, err := kvstore.OpenWithRetry(context.Background(), "bucket", time.Second,
		func(context.Context) (string, error) { calls++; return "kv", nil })

	require.NoError(t, err)
	assert.Equal(t, "kv", got)
	assert.Equal(t, 1, calls, "a bucket that opens must not be retried")
}

// TestOpenWithRetry_SucceedsAfterNotReady is the case the whole helper exists
// for: vpcd starts alongside NATS, and a bucket that is not ready for the first
// few seconds is normal rather than a reason to degrade for the process's life.
func TestOpenWithRetry_SucceedsAfterNotReady(t *testing.T) {
	defer kvstore.SetOpenRetryInterval(time.Millisecond)()

	calls := 0
	got, err := kvstore.OpenWithRetry(context.Background(), "bucket", time.Second,
		func(context.Context) (string, error) {
			calls++
			if calls < 3 {
				return "", errors.New("context deadline exceeded")
			}
			return "kv", nil
		})

	require.NoError(t, err)
	assert.Equal(t, "kv", got)
	assert.Equal(t, 3, calls)
}

func TestOpenWithRetry_GivesUpAndKeepsTheCause(t *testing.T) {
	defer kvstore.SetOpenRetryInterval(time.Millisecond)()
	cause := errors.New("no servers available")

	_, err := kvstore.OpenWithRetry(context.Background(), "spinifex-instance-state", 20*time.Millisecond,
		func(context.Context) (string, error) { return "", cause })

	require.Error(t, err)
	assert.ErrorIs(t, err, cause, "the caller has to be able to see why, not just that")
	assert.Contains(t, err.Error(), "spinifex-instance-state")
}

func TestOpenWithRetry_StopsOnCancelledContext(t *testing.T) {
	defer kvstore.SetOpenRetryInterval(time.Millisecond)()
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	_, err := kvstore.OpenWithRetry(ctx, "bucket", time.Minute,
		func(context.Context) (string, error) {
			calls++
			cancel()
			return "", errors.New("not ready")
		})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls, "a cancelled shutdown must not sit out the window")
}
