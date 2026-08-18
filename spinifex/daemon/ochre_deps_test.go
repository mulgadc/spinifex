// Exercises unexported ochre daemon wiring with no exported surface to
// drive it through.
//
//test:in-package
package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartOchreVector_DisabledSkipsConstruction pins the config gate's
// default-off behavior (D-series Stage 5b): with OchreVector.Enabled false
// (the zero value, i.e. every daemon that has not opted in), startOchreVector
// must construct and subscribe nothing, leaving d.ochreVectorService nil and
// d.natsSubscriptions untouched. No JetStream/NATS connection, ctx or
// natsSubscriptions map is set on d here, so any attempt to construct or
// register past the Enabled check would panic (nil natsConn, nil ctx, or a
// nil-map write) -- the test passing at all is itself part of the pin.
func TestStartOchreVector_DisabledSkipsConstruction(t *testing.T) {
	d := &Daemon{
		config: &config.Config{},
	}

	d.startOchreVector()

	assert.Nil(t, d.ochreVectorService)
	assert.Nil(t, d.natsSubscriptions, "disabled path must not touch the subscriptions map")
}

// TestRetryUntilContext_RetriesPastOldCeilingThenSucceeds proves the connect
// loop no longer gives up after the old fixed ceiling: it keeps retrying until
// the appliance is reachable, so RAG heals once a re-adopted appliance returns.
func TestRetryUntilContext_RetriesPastOldCeilingThenSucceeds(t *testing.T) {
	const failuresBeforeSuccess = 7 // deliberately > the old 5-attempt ceiling

	calls, logged := 0, 0
	got, err := retryUntilContext(context.Background(), time.Millisecond, 2*time.Millisecond,
		func(attempt int, backoff time.Duration, err error) { logged++ },
		func() (int, error) {
			calls++
			if calls <= failuresBeforeSuccess {
				return 0, errors.New("appliance not reachable")
			}
			return 42, nil
		})

	require.NoError(t, err)
	assert.Equal(t, 42, got)
	assert.Equal(t, failuresBeforeSuccess+1, calls, "must retry past the old fixed ceiling")
	assert.Equal(t, failuresBeforeSuccess, logged, "each failure reported to log once")
}

// TestRetryUntilContext_StopsOnContextCancel proves daemon shutdown is the
// loop's exit: a cancel while it is backing off returns context.Canceled
// promptly rather than sleeping out the backoff or starting another attempt.
func TestRetryUntilContext_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	got, err := retryUntilContext(ctx, time.Hour, time.Hour,
		func(attempt int, backoff time.Duration, err error) { cancel() },
		func() (int, error) {
			calls++
			return 0, errors.New("still down")
		})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, got)
	assert.Equal(t, 1, calls, "cancel during backoff stops before the next attempt")
}

// TestHandleOchreApplianceTeardown_NilApplianceRefuses proves the handler
// refuses with a clear error rather than a nil-pointer panic when the
// appliance never came up (disabled, still starting, or already torn down by
// an earlier call) -- it must never touch d.ochreVectorService in that case.
func TestHandleOchreApplianceTeardown_NilApplianceRefuses(t *testing.T) {
	d := &Daemon{}

	out, err := d.handleOchreApplianceTeardown(context.Background(), &handlers_ochrevector.TeardownApplianceRequest{}, "")

	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "not enabled or not up")
}
