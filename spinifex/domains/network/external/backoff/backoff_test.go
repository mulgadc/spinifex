package backoff

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJitter_StaysWithinSpan(t *testing.T) {
	assert.Zero(t, Jitter(0))
	assert.Zero(t, Jitter(-time.Second))
	for range 1000 {
		j := Jitter(10 * time.Millisecond)
		require.GreaterOrEqual(t, j, -10*time.Millisecond)
		require.Less(t, j, 10*time.Millisecond)
	}
}

func TestSleep_ReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, Sleep(ctx, 0), context.Canceled, "a zero wait must still see a cancelled ctx")
	require.ErrorIs(t, Sleep(ctx, time.Hour), context.Canceled)

	require.NoError(t, Sleep(context.Background(), 0))
	require.NoError(t, Sleep(context.Background(), time.Millisecond))
}
