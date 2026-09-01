//test:in-package — pass and keyPass are the decision under test, and driving
//them through Run would turn a pure classification into a timing test against
//DefaultErrorRetry.

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

// A terminal failure repeats identically however often it is retried, so asking
// to be revisited early only produces the same error sooner. The resync stays as
// the backstop, which is why the deadline is left alone rather than suppressed.
func TestPass_ATerminalFailureAsksForNoEarlyRetry(t *testing.T) {
	cfg := Config{
		Name: "terminal",
		Reconcile: func(context.Context) (time.Duration, error) {
			return 0, errors.New(awserrors.ErrorInvalidParameterValue)
		},
	}

	assert.Zero(t, pass(t.Context(), cfg, "test"),
		"a 400 will be a 400 next time; retrying early only logs the same failure sooner")
}

// The case the deadline exists for: a fault that clears on its own should not
// leave the world unconverged until the resync, which is five minutes by default.
func TestPass_ATransientFailureIsRetriedBeforeTheResync(t *testing.T) {
	cfg := Config{
		Name: "transient",
		Reconcile: func(context.Context) (time.Duration, error) {
			return 0, errors.New(awserrors.ErrorServiceUnavailable)
		},
	}

	assert.Equal(t, DefaultErrorRetry, pass(t.Context(), cfg, "test"))
}

// An error carrying no AWS code at all is the common case — a wrapped NATS or
// S3 failure — and IsTerminal calls it transient by design, so it is retried
// rather than silently abandoned.
func TestPass_AnUnclassifiableFailureIsTreatedAsTransient(t *testing.T) {
	cfg := Config{
		Name: "unclassified",
		Reconcile: func(context.Context) (time.Duration, error) {
			return 0, errors.New("read zone: connection reset")
		},
	}

	assert.Equal(t, DefaultErrorRetry, pass(t.Context(), cfg, "test"))
}

// A pass partway through a transition names its own deadline, and that deadline
// is the reason the loop revisits at all. The error retry may bring a revisit
// forward but must never push one back.
func TestPass_ACalleesSoonerDeadlineSurvivesATransientFailure(t *testing.T) {
	cfg := Config{
		Name: "sooner",
		Reconcile: func(context.Context) (time.Duration, error) {
			return 2 * time.Second, errors.New(awserrors.ErrorServiceUnavailable)
		},
	}

	assert.Equal(t, 2*time.Second, pass(t.Context(), cfg, "test"),
		"the callee asked for 2s; the error retry must not defer it to DefaultErrorRetry")
}

// A callee asking for later than the error retry gets the retry instead, which
// is the same direction: sooner wins.
func TestPass_ACalleesLaterDeadlineYieldsToTheErrorRetry(t *testing.T) {
	cfg := Config{
		Name: "later",
		Reconcile: func(context.Context) (time.Duration, error) {
			return time.Hour, errors.New(awserrors.ErrorServiceUnavailable)
		},
	}

	assert.Equal(t, DefaultErrorRetry, pass(t.Context(), cfg, "test"))
}

// The per-key path carries the same classification. A key is otherwise covered
// only by the whole-set resync, so a transient key failure is the case with the
// most to gain from an early revisit.
func TestKeyPass_ClassifiesTheSameWayAsAWholeSetPass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{name: "terminal", err: errors.New(awserrors.ErrorInvalidParameterValue), want: 0},
		{name: "transient", err: errors.New(awserrors.ErrorServiceUnavailable), want: DefaultErrorRetry},
		{name: "unclassifiable", err: errors.New("put key: no responders"), want: DefaultErrorRetry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			cfg := Config{
				Name: "keys",
				ReconcileKey: func(_ context.Context, key string) (time.Duration, error) {
					got = key
					return 0, tt.err
				},
			}

			assert.Equal(t, tt.want, keyPass(t.Context(), cfg, "i-abc"))
			assert.Equal(t, "i-abc", got, "the key that failed has to reach the callee")
		})
	}
}

// A pass that succeeds keeps whatever deadline it named, so the error retry
// cannot leak into the healthy path and make every loop revisit on a timer it
// never asked for.
func TestPass_ASuccessfulPassKeepsItsOwnDeadline(t *testing.T) {
	cfg := Config{
		Name:      "ok",
		Reconcile: func(context.Context) (time.Duration, error) { return 90 * time.Second, nil },
	}

	assert.Equal(t, 90*time.Second, pass(t.Context(), cfg, "test"))
}
