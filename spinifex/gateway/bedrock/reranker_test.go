// Exercises the unexported reranker adapter internals with no exported
// surface to drive them through.
//
//test:in-package
package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRerankProvider_Rerank_RequestShape(t *testing.T) {
	var captured rerankRequest
	var capturedPath, capturedMethod string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"index":0,"score":0.9}]`))
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	_, err := p.Rerank(context.Background(), "what is spinifex?", []string{"spinifex is a control plane"})
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, capturedMethod)
	assert.Equal(t, rerankPath, capturedPath)
	assert.Equal(t, "what is spinifex?", captured.Query)
	assert.Equal(t, []string{"spinifex is a control plane"}, captured.Texts)
}

func TestRerankProvider_Rerank_HappyPathOrdersByDescendingScore(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Deliberately unsorted, and not in request order.
		_, _ = w.Write([]byte(`[
			{"index": 2, "score": 0.1},
			{"index": 0, "score": 0.95},
			{"index": 1, "score": 0.5}
		]`))
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	order, err := p.Rerank(context.Background(), "query", []string{"doc0", "doc1", "doc2"})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, order)
}

func TestRerankProvider_Rerank_EmptyDocsSkipsHTTPCall(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	order, err := p.Rerank(context.Background(), "query", nil)
	require.NoError(t, err)
	assert.Empty(t, order)
	assert.False(t, called, "Rerank must not call the endpoint for an empty doc set")
}

func TestRerankProvider_Rerank_NonOKStatusReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	_, err := p.Rerank(context.Background(), "query", []string{"doc0"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())
}

func TestRerankProvider_Rerank_MalformedJSONReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	_, err := p.Rerank(context.Background(), "query", []string{"doc0"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}

func TestRerankProvider_Rerank_CountMismatchReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"index":0,"score":0.9}]`))
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	_, err := p.Rerank(context.Background(), "query", []string{"doc0", "doc1"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}

func TestRerankProvider_Rerank_OutOfRangeIndexReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"index":5,"score":0.9}]`))
	}))
	defer ts.Close()

	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), modelID)
	p.httpClient = ts.Client()

	_, err := p.Rerank(context.Background(), "query", []string{"doc0"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}

func TestRerankProvider_Rerank_ConnectionRefusedReturnsServiceUnavailable(t *testing.T) {
	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(map[string]string{modelID: "http://127.0.0.1:1"}), modelID)

	_, err := p.Rerank(context.Background(), "query", []string{"doc0"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
}

func TestRerankProvider_Rerank_UnresolvedEndpointReturnsModelNotReady(t *testing.T) {
	modelID := DefaultRerankModel
	p := newRerankProvider(NewStaticEndpointResolver(nil), modelID)

	_, err := p.Rerank(context.Background(), "query", []string{"doc0"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}
