package handlers_imds

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetryGather_CancelledCtx checks the ctx.Done branch of retryBackoff: a
// cancelled context returns fast after the first failure without exhausting retries.
func TestRetryGather_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	wantErr := errors.New("transient")
	out, err := retryGather(ctx, "test", "i-123", func() (*int, error) {
		calls++
		return nil, wantErr
	})

	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, out)
	assert.Equal(t, 1, calls, "cancelled ctx must stop after first attempt")
}

// TestRetryGather_SucceedsAfterRetries checks the retry-then-succeed path returns
// the successful value and stops calling.
func TestRetryGather_SucceedsAfterRetries(t *testing.T) {
	calls := 0
	want := 42
	out, err := retryGather(context.Background(), "test", "i-123", func() (*int, error) {
		calls++
		if calls < imdsLookupRetries {
			return nil, errors.New("transient")
		}
		v := want
		return &v, nil
	})

	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, want, *out)
	assert.Equal(t, imdsLookupRetries, calls)
}

// TestRetryGather_ExhaustsRetries checks a persistent error is returned after
// exactly imdsLookupRetries attempts.
func TestRetryGather_ExhaustsRetries(t *testing.T) {
	calls := 0
	wantErr := errors.New("persistent")
	out, err := retryGather(context.Background(), "test", "i-123", func() (*int, error) {
		calls++
		return nil, wantErr
	})

	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, out)
	assert.Equal(t, imdsLookupRetries, calls)
}
