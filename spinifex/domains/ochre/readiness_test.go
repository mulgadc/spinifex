package ochre

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	// Nothing listens on this URL: every attempt is a transport error, which
	// waitReady must treat as "not yet ready" and keep polling, not fail fast.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := waitReady(ctx, http.DefaultClient, readinessTarget{BaseURL: "http://127.0.0.1:1", Path: "/v1/models"}, 10*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitReady_TEIProbesHealthNotModels(t *testing.T) {
	t.Parallel()
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
	err := waitReady(ctx, ts.Client(), readinessTarget{BaseURL: ts.URL, Path: readinessPath(FamilyTEI)}, 10*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "/health", gotPath)
}

func TestReadinessPath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/v1/models", readinessPath(FamilyMeta))
	assert.Equal(t, "/health", readinessPath(FamilyTEI))
	// An unrecognised family is treated the same as TEI (every non-vLLM
	// engine, mirroring engineForFamily's own default), not as vLLM.
	assert.Equal(t, "/health", readinessPath("some-future-family"))
}

func TestWaitReadyAll_MixedEnginesProbeTheirOwnRoute(t *testing.T) {
	t.Parallel()
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
		"vllm-model": {BaseURL: vllmTS.URL, Path: readinessPath(FamilyMeta)},
		"tei-model":  {BaseURL: teiTS.URL, Path: readinessPath(FamilyTEI)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitReadyAll(ctx, http.DefaultClient, targets, 10*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "/v1/models", vllmPath)
	assert.Equal(t, "/health", teiPath)
}

func TestWaitReadyAll_JoinsPerMemberFailures(t *testing.T) {
	t.Parallel()
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

// countedServer is an httptest server that counts requests and answers each
// with answer, given its 1-based request number.
func countedServer(t *testing.T, answer func(w http.ResponseWriter, n int32)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, hits.Add(1))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func TestWaitReady_RedirectEndsTheWaitUnfollowed(t *testing.T) {
	t.Parallel()
	target, targetHits := countedServer(t, func(w http.ResponseWriter, _ int32) { w.WriteHeader(http.StatusOK) })
	cases := map[string]func(w http.ResponseWriter){}
	for _, code := range []int{301, 302, 303, 307, 308} {
		cases[http.StatusText(code)] = func(w http.ResponseWriter) {
			w.Header().Set("Location", target.URL+"/v1/models")
			w.WriteHeader(code)
		}
	}
	for name, loc := range map[string]string{"unparseable host": "http://[::1", "bad escape": "/v1/%zz"} {
		cases[name] = func(w http.ResponseWriter) {
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
		}
	}
	cases["missing Location"] = func(w http.ResponseWriter) { w.WriteHeader(http.StatusFound) }

	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src, srcHits := countedServer(t, func(w http.ResponseWriter, _ int32) { answer(w) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := waitReady(ctx, src.Client(), readinessTarget{BaseURL: src.URL, Path: "/v1/models"}, 10*time.Millisecond)
			require.ErrorIs(t, err, errReadinessRedirected)
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			assert.EqualValues(t, 1, srcHits.Load(), "a redirect is not retried")
			assert.NotContains(t, err.Error(), target.URL)
			assert.NotContains(t, err.Error(), "[::1")
			assert.NotContains(t, err.Error(), "%zz")
		})
	}
	t.Cleanup(func() { assert.Zero(t, targetHits.Load(), "the redirect target must receive no request") })
}

func TestWaitReady_OrdinaryFailuresKeepPolling(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusNotModified, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			src, hits := countedServer(t, func(w http.ResponseWriter, n int32) {
				if n <= 3 {
					w.WriteHeader(code)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			require.NoError(t, waitReady(ctx, src.Client(), readinessTarget{BaseURL: src.URL, Path: "/health"}, 5*time.Millisecond))
			assert.EqualValues(t, 4, hits.Load())
		})
	}
}

func TestWaitReady_KeepsInjectedClient(t *testing.T) {
	t.Parallel()
	target, targetHits := countedServer(t, func(w http.ResponseWriter, _ int32) { w.WriteHeader(http.StatusOK) })
	src, _ := countedServer(t, func(w http.ResponseWriter, _ int32) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	})
	var transportCalls, policyCalls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})
	policy := func(*http.Request, []*http.Request) error { policyCalls.Add(1); return nil }
	client := &http.Client{Transport: transport, CheckRedirect: policy}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := waitReady(ctx, client, readinessTarget{BaseURL: src.URL, Path: "/v1/models"}, 10*time.Millisecond)
	require.ErrorIs(t, err, errReadinessRedirected)
	assert.EqualValues(t, 1, transportCalls.Load(), "the probe goes through the injected transport")
	assert.Zero(t, policyCalls.Load(), "the injected redirect policy is never consulted")
	assert.Zero(t, targetHits.Load())
	assert.NotNil(t, client.CheckRedirect, "the injected client is unchanged")
	_, isRecorder := client.Transport.(*redirectRecorder)
	assert.False(t, isRecorder, "the injected client's transport is unchanged")
}

func TestWaitReadyAll_RedirectCancelsTheOtherMembers(t *testing.T) {
	t.Parallel()
	redirecting, _ := countedServer(t, func(w http.ResponseWriter, _ int32) { w.WriteHeader(http.StatusTemporaryRedirect) })
	stuck, _ := countedServer(t, func(w http.ResponseWriter, _ int32) { w.WriteHeader(http.StatusServiceUnavailable) })
	targets := map[string]readinessTarget{
		"redirecting-model": {BaseURL: redirecting.URL, Path: "/v1/models"},
		"stuck-model":       {BaseURL: stuck.URL, Path: "/health"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := waitReadyAll(ctx, http.DefaultClient, targets, 5*time.Millisecond)
	require.ErrorIs(t, err, errReadinessRedirected)
	assert.ErrorIs(t, err, context.Canceled, "the other member's wait is cancelled, not left to the deadline")
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.NoError(t, ctx.Err(), "the wait ended long before the caller's deadline")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
