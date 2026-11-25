package ochre

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nomicModel    = "nomic-embed-text-v1.5"
	bgeModel      = "bge-base-en-v1.5"
	rerankerModel = "bge-reranker-v2-m3"

	// teiGuestModelPath is what TEI reports as its model: the guest path the
	// weights were mounted at, which must never reach a caller.
	teiGuestModelPath = "/mnt/weights/nomic-ai/nomic-embed-text-v1.5"
)

// fakeTEI is a TEI server answering /health and /v1/embeddings. By default
// it returns one 3-dim vector per input and a prompt token count of 7 per
// input; status and body override the embeddings reply.
type fakeTEI struct {
	*httptest.Server

	mu       sync.Mutex
	requests []map[string]json.RawMessage
	status   int
	body     string
}

func newFakeTEI(t *testing.T) *fakeTEI {
	t.Helper()
	f := &fakeTEI{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTEI) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != teiEmbeddingsPath || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var req map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	f.requests = append(f.requests, req)
	status, body := f.status, f.body
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	n, _ := countEmbeddingInputs(req["input"])
	data := make([]string, n)
	for i := range n {
		// Reversed order: callers must rely on index, not position.
		idx := n - 1 - i
		data[i] = fmt.Sprintf(`{"object":"embedding","embedding":[%d.5,%s-1],"index":%d}`, idx, nativeWidthFill, idx)
	}
	_, _ = fmt.Fprintf(w, `{"object":"list","data":[%s],"model":%q,"usage":{"prompt_tokens":%d,"total_tokens":%d}}`,
		strings.Join(data, ","), teiGuestModelPath, 7*n, 7*n)
}

// nativeWidthFill is 766 coordinates, so a vector of first, fill and last
// value has the catalog embedders' native width of 768.
var nativeWidthFill = strings.Repeat("0.25,", 766)

// nativeVector is a 768-coordinate JSON array whose first value is first.
func nativeVector(first string) string {
	return "[" + first + "," + nativeWidthFill + "-1]"
}

func (f *fakeTEI) reply(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeTEI) calls() []map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), f.requests...)
}

func (f *fakeTEI) hostPort(t *testing.T) (string, int) {
	t.Helper()
	u, err := url.Parse(f.URL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

func vectorValues(t *testing.T, v []*float64) []float64 {
	t.Helper()
	out := make([]float64, len(v))
	for i, f := range v {
		require.NotNil(t, f)
		out[i] = *f
	}
	return out
}

func embeddingBody(inputs ...string) []byte {
	if len(inputs) == 1 {
		b, _ := json.Marshal(map[string]any{"input": inputs[0]})
		return b
	}
	b, _ := json.Marshal(map[string]any{"input": inputs})
	return b
}

func repeatInputs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "passage " + strconv.Itoa(i)
	}
	return out
}

func staticTEIRouter(t *testing.T, tei *fakeTEI, rec Recorder) *Router {
	t.Helper()
	resolver := NewStaticEndpointResolver(map[string]string{nomicModel: tei.URL, bgeModel: tei.URL, rerankerModel: tei.URL})
	return NewRouter(nil, resolver, rec, grantAll{}, nil, nil, nil)
}

func TestEmbeddingInvoke_ServesBothCatalogEmbeddersFromStaticEndpoint(t *testing.T) {
	for _, modelID := range []string{nomicModel, bgeModel} {
		t.Run(modelID, func(t *testing.T) {
			tei := newFakeTEI(t)
			rec := &fakeRecorder{}
			rt := staticTEIRouter(t, tei, rec)

			body := []byte(`{"input":"search_query: what is mulga?","model":"` + modelID + `","encoding_format":"float","dimensions":null}`)
			respBody, contentType, err := rt.InvokeModel(context.Background(), "000000000001", modelID, body, "", "")
			require.NoError(t, err)
			assert.Equal(t, "application/json", contentType)

			var out teiEmbeddingsResponse
			require.NoError(t, json.Unmarshal(respBody, &out))
			assert.Equal(t, modelID, out.Model, "the catalog ID replaces TEI's own model name")
			assert.NotContains(t, string(respBody), teiGuestModelPath)
			assert.Equal(t, "list", out.Object)
			require.Len(t, out.Data, 1)
			vector := vectorValues(t, out.Data[0].Embedding)
			require.Len(t, vector, 768, "the native width reaches the caller")
			assert.InDelta(t, 0.5, vector[0], 0)
			assert.InDelta(t, 0.25, vector[1], 0)
			assert.InDelta(t, -1.0, vector[767], 0)
			require.NotNil(t, out.Usage)
			assert.Equal(t, int64(7), *out.Usage.PromptTokens)

			calls := tei.calls()
			require.Len(t, calls, 1)
			assert.JSONEq(t, `"search_query: what is mulga?"`, string(calls[0]["input"]))
			assert.JSONEq(t, `"float"`, string(calls[0]["encoding_format"]))
			assert.NotContains(t, calls[0], "model", "the catalog ID is not forwarded to TEI")
			assert.NotContains(t, calls[0], "dimensions")

			records := rec.all()
			require.Len(t, records, 1)
			assert.Equal(t, int64(7), records[0].InputTokens)
			assert.Zero(t, records[0].OutputTokens)
			assert.Empty(t, records[0].InputText)
			assert.Empty(t, records[0].OutputText)
			assert.Empty(t, records[0].ErrorCode)
		})
	}
}

func TestEmbeddingInvoke_BatchLimit(t *testing.T) {
	tei := newFakeTEI(t)
	rec := &fakeRecorder{}
	rt := staticTEIRouter(t, tei, rec)

	respBody, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody(repeatInputs(teiMaxClientBatchSize)...), "", "")
	require.NoError(t, err)
	var out teiEmbeddingsResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	require.Len(t, out.Data, teiMaxClientBatchSize)
	for _, d := range out.Data {
		require.NotNil(t, d.Index)
		assert.InDelta(t, float64(*d.Index)+0.5, vectorValues(t, d.Embedding)[0], 0, "each vector keeps its own index")
	}
	assert.Equal(t, int64(7*teiMaxClientBatchSize), rec.all()[0].InputTokens)

	_, _, err = rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody(repeatInputs(teiMaxClientBatchSize+1)...), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
	assert.Len(t, tei.calls(), 1, "an oversized batch never reaches TEI")
}

func TestEmbeddingInvoke_RejectsInvalidBodiesBeforeTEI(t *testing.T) {
	bodies := map[string]string{
		"malformed json":         `{"input":`,
		"trailing data":          `{"input":"a"} {"input":"b"}`,
		"trailing brace":         `{ "input": "a" } }`,
		"trailing bracket":       `{ "input": "a" } ]`,
		"trailing garbage":       `{"input":"a"}x`,
		"unknown field":          `{"input":"a","truncate":true}`,
		"missing input":          `{}`,
		"null input":             `{"input":null}`,
		"empty string":           `{"input":""}`,
		"empty batch":            `{"input":[]}`,
		"empty batch item":       `{"input":["a",""]}`,
		"non-string batch item":  `{"input":["a",1]}`,
		"token ids":              `{"input":[[1,2,3]]}`,
		"dimensions":             `{"input":"a","dimensions":256}`,
		"base64 encoding":        `{"input":"a","encoding_format":"base64"}`,
		"other model":            `{"input":"a","model":"bge-base-en-v1.5"}`,
		"guest path as model":    `{"input":"a","model":"` + teiGuestModelPath + `"}`,
		"body is not an object":  `"a"`,
		"input is not text json": `{"input":{"text":"a"}}`,
		"null model":             `{"input":"a","model":null}`,
		"null encoding format":   `{"input":"a","encoding_format":null}`,
		"uppercase input key":    `{"Input":"a"}`,
		"uppercase model key":    `{"input":"a","MODEL":"nomic-embed-text-v1.5"}`,
		"mixed-case format key":  `{"input":"a","Encoding_Format":"float"}`,
		"mixed-case dims key":    `{"input":"a","Dimensions":null}`,
		"case-folded duplicate":  `{"input":"a","INPUT":"b"}`,
	}
	tei := newFakeTEI(t)
	rt := staticTEIRouter(t, tei, &fakeRecorder{})
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, []byte(body), "", "")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationException, err.Error())
		})
	}
	assert.Empty(t, tei.calls(), "no invalid body may reach TEI")
}

func TestEmbeddingInvoke_MalformedUpstreamResponseIsModelError(t *testing.T) {
	vec := `"object":"embedding","embedding":` + nativeVector("0.1")
	bodies := map[string]string{
		"not json":                `not json`,
		"missing usage":           `{"data":[{` + vec + `,"index":0}]}`,
		"missing prompt tokens":   `{"data":[{` + vec + `,"index":0}],"usage":{"total_tokens":3}}`,
		"negative prompt tokens":  `{"data":[{` + vec + `,"index":0}],"usage":{"prompt_tokens":-1,"total_tokens":1}}`,
		"wrong vector count":      `{"data":[{` + vec + `,"index":0},{` + vec + `,"index":1}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"missing index":           `{"data":[{` + vec + `}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"out of range index":      `{"data":[{` + vec + `,"index":1}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"null vector element":     `{"data":[{"object":"embedding","embedding":` + nativeVector("null") + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"narrower than native":    `{"data":[{"object":"embedding","embedding":[` + nativeWidthFill + `-1],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"wider than native":       `{"data":[{"object":"embedding","embedding":` + nativeVector("0.1,0.2") + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"empty vector":            `{"data":[{"object":"embedding","embedding":[],"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"missing total tokens":    `{"data":[{` + vec + `,"index":0}],"usage":{"prompt_tokens":1}}`,
		"negative total tokens":   `{"data":[{` + vec + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":-1}}`,
		"string prompt tokens":    `{"data":[{` + vec + `,"index":0}],"usage":{"prompt_tokens":"1","total_tokens":1}}`,
		"missing object":          `{"data":[{"embedding":` + nativeVector("0.1") + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"wrong object":            `{"data":[{"object":"list","embedding":` + nativeVector("0.1") + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"uppercase object":        `{"data":[{"object":"Embedding","embedding":` + nativeVector("0.1") + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		"duplicate index (batch)": `{"data":[{` + vec + `,"index":0},{` + vec + `,"index":0}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			tei := newFakeTEI(t)
			tei.reply(0, body)
			rec := &fakeRecorder{}
			rt := staticTEIRouter(t, tei, rec)

			input := embeddingBody("a")
			if strings.Contains(name, "batch") {
				input = embeddingBody("a", "b")
			}
			_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, input, "", "")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())

			records := rec.all()
			require.Len(t, records, 1)
			assert.Zero(t, records[0].InputTokens, "missing usage is not recorded as a count")
			assert.Empty(t, records[0].InputText)
		})
	}
}

func TestEmbeddingInvoke_MapsUpstreamStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusRequestEntityTooLarge: awserrors.ErrorValidationException,
		http.StatusUnprocessableEntity:   awserrors.ErrorValidationException,
		http.StatusBadRequest:            awserrors.ErrorValidationException,
		http.StatusTooManyRequests:       awserrors.ErrorThrottlingException,
		http.StatusServiceUnavailable:    awserrors.ErrorServiceUnavailableException,
	}
	for status, want := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			tei := newFakeTEI(t)
			tei.reply(status, `{"error":"input quoted: secret passage","error_type":"Validation"}`)
			rt := staticTEIRouter(t, tei, &fakeRecorder{})

			_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("secret passage"), "", "")
			require.Error(t, err)
			assert.Equal(t, want, err.Error())
			assert.NotContains(t, err.Error(), "secret passage")
		})
	}
}

func TestEmbeddingInvoke_DeniedAccountNeverReachesTEI(t *testing.T) {
	tei := newFakeTEI(t)
	resolver := NewStaticEndpointResolver(map[string]string{nomicModel: tei.URL})
	rt := NewRouter(nil, resolver, &fakeRecorder{}, grantSet{bgeModel: true}, nil, nil, nil)

	_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
	assert.Empty(t, tei.calls())
}

// A resolver with no pinned lookup (the PT-scoped spy) cannot serve an
// embedding, and nothing on it is called.
func TestEmbeddingInvoke_ResolverWithoutPinnedLookupIsNotReady(t *testing.T) {
	tei := newFakeTEI(t)
	spy := &spyEndpointResolver{baseURL: tei.URL}
	rt := NewRouter(nil, spy, &fakeRecorder{}, grantAll{}, nil, nil, nil)

	_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
	assert.Empty(t, spy.endpointCalls)
	assert.Empty(t, spy.endpointForAccountCalls)
	assert.Empty(t, tei.calls())
}

// sharedBundleRecord is the ochre-demo bundle with nomic served by tei.
func sharedBundleRecord(t *testing.T, tei *fakeTEI, state EndpointState, pinned bool) EndpointRecord {
	t.Helper()
	host, port := tei.hostPort(t)
	return EndpointRecord{
		AccountID:        awsidentifiers.GlobalAccountID,
		ModelID:          selfHostTestModel,
		State:            state,
		Pinned:           pinned,
		OperatorReserved: pinned,
		PrivateIP:        host,
		BaseURL:          "http://" + net.JoinHostPort(host, "1"),
		Members: map[string]MemberEndpoint{
			selfHostTestModel: {Port: 1, Family: familyMeta},
			nomicModel:        {Port: port, Family: familyTEI},
		},
	}
}

func dynamicTEIRouter(svc *fakeEndpointService, opts ...ResolverOption) (*Router, *DynamicEndpointResolver) {
	resolver := NewDynamicEndpointResolver(svc, nil, 0, opts...)
	return NewRouter(nil, resolver, &fakeRecorder{}, grantAll{}, nil, nil, nil), resolver
}

func TestEmbeddingInvoke_ServesReadyPinnedSharedBundle(t *testing.T) {
	tei := newFakeTEI(t)
	svc := &fakeEndpointService{record: sharedBundleRecord(t, tei, StateReady, true)}
	rt, _ := dynamicTEIRouter(svc)

	respBody, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a", "b"), "", "")
	require.NoError(t, err)
	var out teiEmbeddingsResponse
	require.NoError(t, json.Unmarshal(respBody, &out))
	assert.Len(t, out.Data, 2)
	assert.Len(t, tei.calls(), 1, "the request reached nomic's own member port")
	assert.Equal(t, awsidentifiers.GlobalAccountID, svc.describeAccount())
	assert.Zero(t, svc.ensureCalls.Load())
}

// Anything short of a READY record an operator pinned, carrying this member,
// is not served, and the lookup never starts or waits for an endpoint even
// with a nonzero cold-start wait configured.
func TestEmbeddingInvoke_LookupOnlyRefusesUnservableRecords(t *testing.T) {
	cases := map[string]func(*testing.T, *fakeTEI) EndpointRecord{
		"absent": func(*testing.T, *fakeTEI) EndpointRecord {
			return EndpointRecord{ModelID: nomicModel, State: StateAbsent}
		},
		"ready unpinned": func(t *testing.T, tei *fakeTEI) EndpointRecord {
			return sharedBundleRecord(t, tei, StateReady, false)
		},
		"ready pinned by a commitment, not an operator": func(t *testing.T, tei *fakeTEI) EndpointRecord {
			rec := sharedBundleRecord(t, tei, StateReady, true)
			rec.OperatorReserved = false
			return rec
		},
		"starting pinned": func(t *testing.T, tei *fakeTEI) EndpointRecord {
			return sharedBundleRecord(t, tei, StateStarting, true)
		},
		"draining pinned": func(t *testing.T, tei *fakeTEI) EndpointRecord {
			return sharedBundleRecord(t, tei, StateDraining, true)
		},
		"pinned without this member": func(t *testing.T, tei *fakeTEI) EndpointRecord {
			rec := sharedBundleRecord(t, tei, StateReady, true)
			delete(rec.Members, nomicModel)
			rec.BaseURL = tei.URL
			return rec
		},
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			tei := newFakeTEI(t)
			svc := &fakeEndpointService{record: record(t, tei)}
			rt, _ := dynamicTEIRouter(svc, WithColdStartWait(time.Hour))

			start := time.Now()
			_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
			assert.Less(t, time.Since(start), time.Minute, "the lookup must not wait out the cold-start window")
			assert.Zero(t, svc.ensureCalls.Load(), "an embedding lookup never starts an endpoint")
			assert.Positive(t, svc.describeCalls.Load())
			assert.Empty(t, tei.calls())
		})
	}
}

func TestEmbeddingInvoke_DescribeFailureIsNotServed(t *testing.T) {
	tei := newFakeTEI(t)
	svc := &fakeEndpointService{describeErr: errors.New("nats: no responders")}
	rt, _ := dynamicTEIRouter(svc)

	_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, resolveEndpointError(svc.describeErr).Error(), err.Error())
	assert.Zero(t, svc.ensureCalls.Load())
	assert.Empty(t, tei.calls())
}

// The shared URL cache records reachability, not a pin: a URL cached by an
// on-demand Endpoint call must not let an unpinned bundle serve embeddings.
func TestEmbeddingInvoke_UnpinnedCachedURLIsNotUsed(t *testing.T) {
	tei := newFakeTEI(t)
	svc := &fakeEndpointService{record: sharedBundleRecord(t, tei, StateReady, false)}
	rt, resolver := dynamicTEIRouter(svc)

	cached, ok, err := resolver.Endpoint(context.Background(), nomicModel)
	require.NoError(t, err)
	require.True(t, ok, "precondition: the on-demand path cached the unpinned member URL")
	require.NotEmpty(t, cached)
	ensures := svc.ensureCalls.Load()

	_, _, err = rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
	assert.Equal(t, ensures, svc.ensureCalls.Load())
	assert.Empty(t, tei.calls())
}

// Unsupported embedding operations are refused with ValidationException
// while the model's admission is saturated, proving they are decided first.
func TestEmbeddingInvoke_RefusalsPrecedeAdmission(t *testing.T) {
	tei := newFakeTEI(t)
	rt := staticTEIRouter(t, tei, &fakeRecorder{})

	releaseAll := saturate(t, admissionKey("", nomicModel), teiEmbeddingMaxConcurrency)

	_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "gr-123", "1")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error(), "guardrail")

	_, err = rt.InvokeModelWithResponseStream(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error(), "stream")

	_, err = rt.Converse(context.Background(), "000000000001", nomicModel, converseInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error(), "converse")

	_, err = rt.ConverseStream(context.Background(), "000000000001", nomicModel, converseStreamInput())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error(), "converse stream")

	assert.Empty(t, tei.calls())

	// The same saturation throttles a supported call, so the refusals above
	// were decided ahead of admission rather than instead of it.
	_, _, err = rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())
	assert.Empty(t, tei.calls())

	releaseAll()
	_, _, err = rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.NoError(t, err)
	assert.Len(t, tei.calls(), 1)
}

// saturate holds n admission slots on key until cleanup or the returned
// release runs.
func saturate(t *testing.T, key string, n int) func() {
	t.Helper()
	var releases []func()
	for range n {
		release, ok := selfHostLimiter.Acquire(key, teiEmbeddingMaxConcurrency)
		require.True(t, ok)
		releases = append(releases, release)
	}
	releaseAll := func() {
		for _, r := range releases {
			r()
		}
		releases = nil
	}
	t.Cleanup(releaseAll)
	return releaseAll
}

func TestEmbeddingInvoke_AdmissionCapacityIsSixteenPerGateway(t *testing.T) {
	tei := newFakeTEI(t)
	rt := staticTEIRouter(t, tei, &fakeRecorder{})

	saturate(t, admissionKey("", bgeModel), 15)
	_, _, err := rt.InvokeModel(context.Background(), "000000000001", bgeModel, embeddingBody("a"), "", "")
	require.NoError(t, err, "a sixteenth concurrent request is admitted")

	saturate(t, admissionKey("", bgeModel), 1)
	_, _, err = rt.InvokeModel(context.Background(), "000000000001", bgeModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error(), "a seventeenth is throttled")
	assert.Len(t, tei.calls(), 1)
}

func TestEmbeddingInvoke_RerankEntryStaysRefused(t *testing.T) {
	tei := newFakeTEI(t)
	rt := staticTEIRouter(t, tei, &fakeRecorder{})

	_, _, err := rt.InvokeModel(context.Background(), "000000000001", rerankerModel, []byte(`{"query":"q","texts":["a"]}`), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
	assert.Empty(t, tei.calls())
}

// A tenant commitment on an embedding model is refused before admission or
// any endpoint resolution.
func TestEmbeddingInvoke_TenantProvisionedARNRefused(t *testing.T) {
	tei := newFakeTEI(t)
	store := newProvisionedTestStore(t, newStubEndpointProvisioner())
	out, err := CreateProvisionedModelThroughput(context.Background(), ptCallerAccount, store, createInput(nomicModel, "embed-pt", 1))
	require.NoError(t, err)
	arn := *out.ProvisionedModelArn

	saturate(t, admissionKey(ptCallerAccount, nomicModel), teiEmbeddingMaxConcurrency)

	spy := &spyEndpointResolver{baseURL: tei.URL}
	rt := NewRouter(nil, spy, &fakeRecorder{}, grantAll{}, store, nil, nil)

	_, _, err = rt.InvokeModel(context.Background(), ptCallerAccount, arn, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
	assert.Empty(t, spy.endpointCalls)
	assert.Empty(t, spy.endpointForAccountCalls)
	assert.Empty(t, tei.calls())
}

func TestEmbeddingInvoke_ConverseRefusedBeforeEndpoint(t *testing.T) {
	for _, modelID := range []string{nomicModel, bgeModel} {
		t.Run(modelID, func(t *testing.T) {
			spy := &spyEndpointResolver{baseURL: "http://unused:1"}
			rt := NewRouter(nil, spy, &fakeRecorder{}, grantAll{}, nil, nil, nil)

			_, err := rt.Converse(context.Background(), "000000000001", modelID, converseInput())
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationException, err.Error())

			_, err = rt.ConverseStream(context.Background(), "000000000001", modelID, converseStreamInput())
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorValidationException, err.Error())

			assert.Empty(t, spy.endpointCalls)
			assert.Empty(t, spy.endpointForAccountCalls)
		})
	}
}

// The production recorder publishes usage for an embedding call but neither
// the source text nor the vectors, even with text delivery enabled.
func TestEmbeddingInvoke_StreamRecorderCarriesUsageWithoutBodies(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	_, err := EnsureInvocationStream(ctx, js)
	require.NoError(t, err)
	consumer, err := EnsureDeliveryConsumer(ctx, js)
	require.NoError(t, err)

	recorder := NewStreamRecorder(js, stubLoggingConfigReader{cfg: LoggingConfig{TextDataDeliveryEnabled: true, EmbeddingDataDeliveryEnabled: true}, ok: true})
	tei := newFakeTEI(t)
	rt := staticTEIRouter(t, tei, recorder)

	const passage = "confidential passage text"
	_, _, err = rt.InvokeModel(ctx, "000000000001", nomicModel, embeddingBody(passage, "second"), "", "")
	require.NoError(t, err)

	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
	require.NoError(t, err)
	var raw []byte
	for msg := range batch.Messages() {
		raw = msg.Data()
		require.NoError(t, msg.Ack())
	}
	require.NoError(t, batch.Error())
	require.NotEmpty(t, raw)

	var rec InvocationRecord
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, nomicModel, rec.ModelID)
	assert.Equal(t, int64(14), rec.InputTokens)
	assert.Zero(t, rec.OutputTokens)
	assert.Empty(t, rec.InputText)
	assert.Empty(t, rec.OutputText)
	assert.NotContains(t, string(raw), passage)
	assert.NotContains(t, string(raw), "0.25", "no vector component reaches the stream")
	assert.Zero(t, recorder.Dropped())
}

// A denied embedding request records no body either, even with text delivery
// enabled: the model is classified before the grant can refuse it.
func TestEmbeddingInvoke_DeniedRequestRecordsNoBody(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	_, err := EnsureInvocationStream(ctx, js)
	require.NoError(t, err)
	consumer, err := EnsureDeliveryConsumer(ctx, js)
	require.NoError(t, err)

	recorder := NewStreamRecorder(js, stubLoggingConfigReader{cfg: LoggingConfig{TextDataDeliveryEnabled: true}, ok: true})
	tei := newFakeTEI(t)
	resolver := NewStaticEndpointResolver(map[string]string{nomicModel: tei.URL})
	rt := NewRouter(nil, resolver, recorder, grantSet{}, nil, nil, nil)

	const passage = "confidential denied passage"
	_, _, err = rt.InvokeModel(ctx, "000000000001", nomicModel, embeddingBody(passage), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDeniedException, err.Error())
	assert.Empty(t, tei.calls())

	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
	require.NoError(t, err)
	var raw []byte
	for msg := range batch.Messages() {
		raw = msg.Data()
		require.NoError(t, msg.Ack())
	}
	require.NoError(t, batch.Error())
	require.NotEmpty(t, raw)

	var rec InvocationRecord
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, awserrors.ErrorAccessDeniedException, rec.ErrorCode)
	assert.Empty(t, rec.InputText)
	assert.NotContains(t, string(raw), passage)
}

// A transport failure logs the endpoint by scheme and host and a fixed
// failure label, never the URL's userinfo or the raw client error.
func TestEmbeddingInvoke_TransportFailureLogOmitsUserinfo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	var logBuf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	resolver := NewStaticEndpointResolver(map[string]string{nomicModel: "http://alice:s3cret-token@" + addr})
	rt := NewRouter(nil, resolver, &fakeRecorder{}, grantAll{}, nil, nil, nil)
	_, _, err = rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("a"), "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())

	logs := logBuf.String()
	require.Contains(t, logs, `"endpoint":"http://`+addr+`"`)
	assert.Contains(t, logs, `"failure":"unreachable"`)
	assert.NotContains(t, logs, "alice")
	assert.NotContains(t, logs, "s3cret-token")
}

// A redirect from the TEI endpoint is answered as an upstream failure and
// never followed, so the embedding input cannot reach another host.
func TestEmbeddingInvoke_DoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var elsewhere sync.Mutex
			var elsewhereCalls int
			other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				elsewhere.Lock()
				defer elsewhere.Unlock()
				elsewhereCalls++
			}))
			t.Cleanup(other.Close)
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+r.URL.Path, status)
			}))
			t.Cleanup(redirector.Close)

			resolver := NewStaticEndpointResolver(map[string]string{nomicModel: redirector.URL})
			rt := NewRouter(nil, resolver, &fakeRecorder{}, grantAll{}, nil, nil, nil)
			_, _, err := rt.InvokeModel(context.Background(), "000000000001", nomicModel, embeddingBody("search_document: secret"), "", "")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorModelErrorException, err.Error())
			elsewhere.Lock()
			defer elsewhere.Unlock()
			assert.Zero(t, elsewhereCalls, "the redirect target must receive nothing")
		})
	}
}
