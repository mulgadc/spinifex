package handlers_bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitReady_ImmediateSuccess(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/v1/models", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitReady(ctx, ts.Client(), ts.URL, 10*time.Millisecond)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, requests.Load(), int32(1))
}

func TestWaitReady_NonOKThenOK(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitReady(ctx, ts.Client(), ts.URL, 10*time.Millisecond)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, requests.Load(), int32(3))
}

func TestWaitReady_TimeoutNeverReady(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitReady(ctx, ts.Client(), ts.URL, 10*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitReady_ConnectionRefusedRetriedNotFailed(t *testing.T) {
	// Nothing listens on this URL: every attempt is a transport error, which
	// waitReady must treat as "not yet ready" and keep polling, not fail fast.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitReady(ctx, http.DefaultClient, "http://127.0.0.1:1", 10*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
