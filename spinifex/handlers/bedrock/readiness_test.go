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

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
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
	err := waitReady(ctx, ts.Client(), readinessTarget{BaseURL: ts.URL, Path: "/v1/models"}, 10*time.Millisecond)
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
	err := waitReady(ctx, ts.Client(), readinessTarget{BaseURL: ts.URL, Path: "/v1/models"}, 10*time.Millisecond)
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
	err := waitReady(ctx, ts.Client(), readinessTarget{BaseURL: ts.URL, Path: "/v1/models"}, 10*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitReady_ConnectionRefusedRetriedNotFailed(t *testing.T) {
	// Nothing listens on this URL: every attempt is a transport error, which
	// waitReady must treat as "not yet ready" and keep polling, not fail fast.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitReady(ctx, http.DefaultClient, readinessTarget{BaseURL: "http://127.0.0.1:1", Path: "/v1/models"}, 10*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitReady_TEIProbesHealthNotModels(t *testing.T) {
	// A TEI member's readinessTarget must poll /health, never /v1/models --
	// TEI's own surface has no such route.
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitReady(ctx, ts.Client(), readinessTarget{BaseURL: ts.URL, Path: readinessPath(gateway_bedrock.FamilyTEI)}, 10*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "/health", gotPath)
}

func TestReadinessPath(t *testing.T) {
	assert.Equal(t, "/v1/models", readinessPath(gateway_bedrock.FamilyMeta))
	assert.Equal(t, "/health", readinessPath(gateway_bedrock.FamilyTEI))
	// An unrecognised family is treated the same as TEI (every non-vLLM
	// engine, mirroring engineForFamily's own default), not as vLLM.
	assert.Equal(t, "/health", readinessPath("some-future-family"))
}

func TestWaitReadyAll_MixedEnginesProbeTheirOwnRoute(t *testing.T) {
	var vllmPath, teiPath string
	vllmTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vllmPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer vllmTS.Close()
	teiTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		teiPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer teiTS.Close()

	targets := map[string]readinessTarget{
		"vllm-model": {BaseURL: vllmTS.URL, Path: readinessPath(gateway_bedrock.FamilyMeta)},
		"tei-model":  {BaseURL: teiTS.URL, Path: readinessPath(gateway_bedrock.FamilyTEI)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitReadyAll(ctx, http.DefaultClient, targets, 10*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "/v1/models", vllmPath)
	assert.Equal(t, "/health", teiPath)
}

func TestWaitReadyAll_JoinsPerMemberFailures(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	targets := map[string]readinessTarget{
		"stuck-model": {BaseURL: ts.URL, Path: "/health"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitReadyAll(ctx, ts.Client(), targets, 10*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stuck-model")
}
