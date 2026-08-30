// Exercises the unexported embeddings adapter internals with no exported
// surface to drive them through.
//
//test:in-package
package gateway_bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingsProvider_Embed_RequestShape(t *testing.T) {
	var captured embeddingsRequest
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
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}],"model":"nomic-embed-text-v1.5","usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	_, err := p.Embed(context.Background(), modelID, []string{"hello world"})
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, capturedMethod)
	assert.Equal(t, embeddingsPath, capturedPath)
	assert.Equal(t, modelID, captured.Model)
	assert.Equal(t, []string{"hello world"}, captured.Input)
}

func TestEmbeddingsProvider_Embed_HappyPathReordersToInputOrder(t *testing.T) {
	var captured embeddingsRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Deliberately out of order relative to the request's input batch.
		_, _ = w.Write([]byte(`{
			"data": [
				{"index": 2, "embedding": [2.0, 2.1, 2.2]},
				{"index": 0, "embedding": [0.0, 0.1, 0.2]},
				{"index": 1, "embedding": [1.0, 1.1, 1.2]}
			],
			"model": "nomic-embed-text-v1.5",
			"usage": {"prompt_tokens": 3, "total_tokens": 3}
		}`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	inputs := []string{"first", "second", "third"}
	vectors, err := p.Embed(context.Background(), modelID, inputs)
	require.NoError(t, err)

	assert.Equal(t, inputs, captured.Input)
	require.Len(t, vectors, 3)
	assert.Equal(t, []float32{0.0, 0.1, 0.2}, vectors[0])
	assert.Equal(t, []float32{1.0, 1.1, 1.2}, vectors[1])
	assert.Equal(t, []float32{2.0, 2.1, 2.2}, vectors[2])
	for _, v := range vectors {
		assert.Len(t, v, 3)
	}
}

func TestEmbeddingsProvider_Embed_SplitsOversizedInputIntoBoundedSubBatches(t *testing.T) {
	const totalInputs = 2*maxEmbedClientBatch + 6 // 70 -> ceil(70/32) == 3 requests: 32, 32, 6.

	var requestInputCounts []int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingsRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		requestInputCounts = append(requestInputCounts, len(req.Input))
		if !assert.LessOrEqual(t, len(req.Input), maxEmbedClientBatch, "every sub-batch request must respect maxEmbedClientBatch") {
			http.Error(w, "oversized sub-batch", http.StatusBadRequest)
			return
		}

		// Encode each input's absolute position (parsed from its own text) into
		// the returned embedding, so the caller-side reassembly can be checked
		// independently of this request's local index ordering.
		data := make([]embeddingsDatum, len(req.Input))
		for localIdx, input := range req.Input {
			var absIdx int
			if _, err := fmt.Sscanf(input, "chunk-%d", &absIdx); !assert.NoError(t, err) {
				http.Error(w, "parse input position", http.StatusBadRequest)
				return
			}
			data[localIdx] = embeddingsDatum{Index: localIdx, Embedding: []float32{float32(absIdx)}}
		}

		resp := embeddingsResponse{Data: data, Model: DefaultEmbeddingModel}
		respBody, err := json.Marshal(resp)
		if !assert.NoError(t, err) {
			http.Error(w, "marshal response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	inputs := make([]string, totalInputs)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("chunk-%d", i)
	}

	vectors, err := p.Embed(context.Background(), modelID, inputs)
	require.NoError(t, err)

	require.Equal(t, []int{maxEmbedClientBatch, maxEmbedClientBatch, 6}, requestInputCounts,
		"Embed must issue one request per sub-batch, each bounded by maxEmbedClientBatch")

	require.Len(t, vectors, totalInputs)
	for i, v := range vectors {
		require.Equal(t, []float32{float32(i)}, v, "vector at position %d must be reassembled in original input order", i)
	}
}

func TestEmbeddingsProvider_Embed_EmptyInputSkipsHTTPCall(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	vectors, err := p.Embed(context.Background(), modelID, nil)
	require.NoError(t, err)
	assert.Empty(t, vectors)
	assert.False(t, called, "Embed must not call the endpoint for an empty input batch")
}

func TestEmbeddingsProvider_Embed_ResolverMissReturnsErrorWithoutHTTPCall(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	p := newEmbeddingsProvider(NewStaticEndpointResolver(nil))
	p.httpClient = ts.Client()

	_, err := p.Embed(context.Background(), DefaultEmbeddingModel, []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
	assert.False(t, called, "Embed must not call the endpoint when the resolver misses")
}

func TestEmbeddingsProvider_Embed_NonOKStatusReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	_, err := p.Embed(context.Background(), modelID, []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())
}

func TestEmbeddingsProvider_Embed_MalformedJSONReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	_, err := p.Embed(context.Background(), modelID, []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}

func TestEmbeddingsProvider_Embed_CountMismatchReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"model":"nomic-embed-text-v1.5"}`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	_, err := p.Embed(context.Background(), modelID, []string{"hello", "world"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
}

func TestEmbeddingsProvider_MaxInputLength_ReadsAndCachesInfo(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, infoPath, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model_id":"bge-base-en-v1.5","max_input_length":512}`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	assert.Equal(t, 512, p.MaxInputLength(context.Background(), modelID))
	// A second call for the same endpoint must be served from cache, not a
	// second /info round trip.
	assert.Equal(t, 512, p.MaxInputLength(context.Background(), modelID))
	assert.Equal(t, 1, calls, "MaxInputLength must cache per endpoint")
}

func TestEmbeddingsProvider_MaxInputLength_InfoUnavailableFallsBackToDefault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	assert.Equal(t, defaultMaxInputLength, p.MaxInputLength(context.Background(), modelID))
}

func TestEmbeddingsProvider_MaxInputLength_MalformedInfoFallsBackToDefault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	assert.Equal(t, defaultMaxInputLength, p.MaxInputLength(context.Background(), modelID))
}

func TestEmbeddingsProvider_MaxInputLength_ZeroFieldFallsBackToDefault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// No max_input_length field at all, e.g. a non-TEI OpenAI-compatible
		// /info that doesn't advertise one.
		_, _ = w.Write([]byte(`{"model_id":"some-model"}`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	assert.Equal(t, defaultMaxInputLength, p.MaxInputLength(context.Background(), modelID))
}

func TestEmbeddingsProvider_MaxInputLength_ResolverMissFallsBackToDefault(t *testing.T) {
	p := newEmbeddingsProvider(NewStaticEndpointResolver(nil))
	assert.Equal(t, defaultMaxInputLength, p.MaxInputLength(context.Background(), DefaultEmbeddingModel))
}

func TestEmbeddingsProvider_CountTokens_HappyPath(t *testing.T) {
	var capturedBody map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, tokenizePath, r.URL.Path)
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":101,"text":"[CLS]"},{"id":7592,"text":"hello"},{"id":2088,"text":"world"},{"id":102,"text":"[SEP]"}]`))
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	count, ok := p.CountTokens(context.Background(), modelID, "hello world")
	require.True(t, ok)
	assert.Equal(t, 4, count)
	assert.Equal(t, "hello world", capturedBody["inputs"])
}

func TestEmbeddingsProvider_CountTokens_UnavailableReturnsNotOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	count, ok := p.CountTokens(context.Background(), modelID, "hello world")
	assert.False(t, ok)
	assert.Zero(t, count)
}

func TestEmbeddingsProvider_CountTokens_ResolverMissReturnsNotOK(t *testing.T) {
	p := newEmbeddingsProvider(NewStaticEndpointResolver(nil))
	count, ok := p.CountTokens(context.Background(), DefaultEmbeddingModel, "hello world")
	assert.False(t, ok)
	assert.Zero(t, count)
}

// countingErrTransport fails the first failures RoundTrip calls with err,
// then delegates to next -- lets a test control exactly how many transport
// failures embedBatch's retry must absorb before (or whether) it succeeds.
type countingErrTransport struct {
	failures int
	err      error
	next     http.RoundTripper
	calls    int
}

func (t *countingErrTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	if t.calls <= t.failures {
		return nil, t.err
	}
	return t.next.RoundTrip(req)
}

func TestEmbeddingsProvider_EmbedBatch_RetriesTransportErrorThenSucceeds(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"model":"nomic-embed-text-v1.5","usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer ts.Close()

	transport := &countingErrTransport{
		failures: embedTransportRetries, // fail every attempt but the last
		err:      &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		next:     ts.Client().Transport,
	}

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = &http.Client{Transport: transport}

	vectors, err := p.Embed(context.Background(), modelID, []string{"hello"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Equal(t, embedTransportRetries+1, transport.calls,
		"must retry the connection-refused transport error up to the cap before succeeding")
}

func TestEmbeddingsProvider_EmbedBatch_GivesUpAfterTransportRetryCap(t *testing.T) {
	transport := &countingErrTransport{
		failures: embedTransportRetries + 10, // always fail
		err:      &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		next:     http.DefaultTransport,
	}

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: "http://127.0.0.1:1"}))
	p.httpClient = &http.Client{Transport: transport}

	_, err := p.Embed(context.Background(), modelID, []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
	assert.Equal(t, embedTransportRetries+1, transport.calls,
		"must give up after embedTransportRetries+1 attempts, not retry forever")
}

func TestEmbeddingsProvider_EmbedBatch_NonTransportErrorNotRetried(t *testing.T) {
	transport := &countingErrTransport{
		failures: embedTransportRetries + 10, // always fail
		err:      errors.New("boom: not a transport error"),
		next:     http.DefaultTransport,
	}

	modelID := DefaultEmbeddingModel
	p := newEmbeddingsProvider(NewStaticEndpointResolver(map[string]string{modelID: "http://127.0.0.1:1"}))
	p.httpClient = &http.Client{Transport: transport}

	_, err := p.Embed(context.Background(), modelID, []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
	assert.Equal(t, 1, transport.calls, "a non-transport error must not be retried")
}
