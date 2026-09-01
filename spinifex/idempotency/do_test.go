package idempotency_test

import (
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The property the whole feature rests on: one token, two calls, the work runs
// once and both callers get the same result.
func TestDo_RunsWorkOncePerToken(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok, hash = "do-1", "h"
	runs := 0
	work := func() (payload, error) {
		runs++
		return payload{ID: "r-1"}, nil
	}

	first, err := idempotency.Do(t.Context(), store, testAccount, tok, hash, work)
	require.NoError(t, err)
	second, err := idempotency.Do(t.Context(), store, testAccount, tok, hash, work)
	require.NoError(t, err)

	assert.Equal(t, 1, runs, "the duplicate must replay, not re-run the work")
	assert.Equal(t, "r-1", first.ID)
	assert.Equal(t, first, second, "the duplicate returns the first call's result")
}

// A failed attempt must not be replayed as if it had succeeded: the token is
// released so a retry gets a real attempt.
func TestDo_FailedWorkIsRetryable(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok, hash = "do-2", "h"
	boom := errors.New("create failed")

	_, err := idempotency.Do(t.Context(), store, testAccount, tok, hash, func() (payload, error) {
		return payload{}, boom
	})
	require.ErrorIs(t, err, boom, "the work's own error passes through unwrapped")

	runs := 0
	out, err := idempotency.Do(t.Context(), store, testAccount, tok, hash, func() (payload, error) {
		runs++
		return payload{ID: "r-2"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, runs, "the retry re-runs the work")
	assert.Equal(t, "r-2", out.ID)
}

// Reusing a token with different parameters is the caller's error, and must not
// run the work at all.
func TestDo_ParamMismatchDoesNotRunWork(t *testing.T) {
	t.Parallel()
	store := newTestStore[payload](t)
	const tok = "do-3"

	_, err := idempotency.Do(t.Context(), store, testAccount, tok, "hash-a", func() (payload, error) {
		return payload{ID: "r-3"}, nil
	})
	require.NoError(t, err)

	ran := false
	_, err = idempotency.Do(t.Context(), store, testAccount, tok, "hash-b", func() (payload, error) {
		ran = true
		return payload{}, nil
	})
	assert.ErrorIs(t, err, idempotency.ErrParamMismatch)
	assert.False(t, ran, "a mismatched token must not do the work")
}

// Callers map every non-mismatch token failure onto one API error, so they all
// have to be reachable through ErrUnavailable.
func TestTokenFailuresAreUnavailable(t *testing.T) {
	t.Parallel()
	assert.ErrorIs(t, idempotency.ErrWaitTimeout, idempotency.ErrUnavailable)
	assert.ErrorIs(t, idempotency.ErrNoReplay, idempotency.ErrUnavailable)
	assert.NotErrorIs(t, idempotency.ErrParamMismatch, idempotency.ErrUnavailable)
}
