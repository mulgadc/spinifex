// Exercises warmupProbe's background readiness cache directly, with no
// exported surface to drive it through.
//
//test:in-package
package gateway_bedrock

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWarmupProbe_NotReadyUntilHealthPasses(t *testing.T) {
	var healthy atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, embedderHealthPath, r.URL.Path)
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	p := newWarmupProbeWithInterval(ts.Client(), ts.URL, 10*time.Millisecond)

	// The immediate first probe run issues before the ticker starts lands
	// against the not-yet-healthy server; give it a moment to complete.
	time.Sleep(30 * time.Millisecond)
	assert.False(t, p.Ready(), "must report not-ready until /health passes")

	healthy.Store(true)
	require.Eventually(t, p.Ready, time.Second, 5*time.Millisecond,
		"must become ready once /health starts returning 200")
}

func TestWarmupProbe_ReProbesAfterInterval(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	p := newWarmupProbeWithInterval(ts.Client(), ts.URL, 10*time.Millisecond)
	require.Eventually(t, p.Ready, time.Second, 5*time.Millisecond,
		"must become ready once the endpoint is healthy")

	callsAtReady := calls.Load()
	healthy.Store(false)
	require.Eventually(t, func() bool { return !p.Ready() }, time.Second, 5*time.Millisecond,
		"must re-probe on the next tick and flip back to not-ready")
	assert.Greater(t, calls.Load(), callsAtReady, "must have issued at least one more probe after the first ready result")
}

func TestEmbeddingsProvider_Embed_WarmupGateFailsFastUntilHealthy(t *testing.T) {
	var healthy atomic.Bool
	var embedCalls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case embedderHealthPath:
			if healthy.Load() {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		case embeddingsPath:
			embedCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"model":"nomic-embed-text-v1.5","usage":{"prompt_tokens":1,"total_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()
	p.warmupPollInterval = 10 * time.Millisecond
	p.warmupFor(ts.URL) // mirrors NewEmbedder(resolver, ts.URL)

	time.Sleep(30 * time.Millisecond)
	_, err := p.Embed(t.Context(), modelID, []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
	assert.Zero(t, embedCalls.Load(), "must not dial the embeddings endpoint while the warm-up gate reports not ready")

	healthy.Store(true)
	require.Eventually(t, func() bool {
		_, err := p.Embed(t.Context(), modelID, []string{"hello"})
		return err == nil
	}, time.Second, 10*time.Millisecond, "must succeed once the warm-up probe catches up to the endpoint being healthy")
}
