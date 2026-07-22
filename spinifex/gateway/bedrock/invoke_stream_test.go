package gateway_bedrock

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInvokeStreamSource drives pumpInvokeStream directly with a scripted
// chunk/error sequence, independent of any real backend.
type fakeInvokeStreamSource struct {
	chunks [][]byte
	err    error
	closed bool
}

func (f *fakeInvokeStreamSource) Next(_ context.Context) ([]byte, bool, error) {
	if len(f.chunks) > 0 {
		c := f.chunks[0]
		f.chunks = f.chunks[1:]
		return c, true, nil
	}
	if f.err != nil {
		return nil, false, f.err
	}
	return nil, false, nil
}

func (f *fakeInvokeStreamSource) Close() error {
	f.closed = true
	return nil
}

func TestPumpInvokeStream_MidStreamFaultWritesExceptionFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	src := &fakeInvokeStreamSource{
		chunks: [][]byte{[]byte(`{"generation":"hi"}`)},
		err:    newStreamFault(errors.New("upstream connection reset")),
	}
	pumpInvokeStream(context.Background(), fw, src, "test-model")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 2)
	assert.Equal(t, "chunk", frames[0].Type)
	assert.Equal(t, "exception", frames[1].MessageType)
	assert.Equal(t, excModelStreamErrorException, frames[1].Type)
}

func TestPumpInvokeStream_NonFaultErrorUsesInternalServerException(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	src := &fakeInvokeStreamSource{err: errors.New("plain internal error")}
	pumpInvokeStream(context.Background(), fw, src, "test-model")

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 1)
	assert.Equal(t, excInternalServerException, frames[0].Type)
}

func TestInvokeModelWithResponseStream_UnknownModelReturnsResourceNotFoundPreHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	err := InvokeModelWithResponseStream(context.Background(), rec, "000000000001", "does.not-exist-v1:0", []byte(`{}`), nil, nil, grantAll{}, "application/json")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFoundException, err.Error())
	assert.Equal(t, 0, rec.Body.Len())
}

func TestInvokeModelWithResponseStream_SelfHostHappyPath_WritesChunkFrames(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	rec := httptest.NewRecorder()
	body := []byte(`{"prompt":"hello","max_gen_len":128}`)

	err := InvokeModelWithResponseStream(context.Background(), rec, "000000000001", modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, "application/json")
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, eventStreamContentType, rec.Header().Get("Content-Type"))

	frames := decodeAllFrames(t, rec.Body.Bytes())
	require.Len(t, frames, 3)
	for _, f := range frames {
		assert.Equal(t, "chunk", f.Type)
	}
}

func TestInvokeModelWithResponseStream_NonFlusherWriter_ReturnsPreHeaderError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(llamaCompletionsStreamFixture))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	w := &nonFlusherWriter{}
	body := []byte(`{"prompt":"hello"}`)

	err := InvokeModelWithResponseStream(context.Background(), w, "000000000001", modelID, body, nil, NewStaticEndpointResolver(map[string]string{modelID: ts.URL}), grantAll{}, "application/json")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.False(t, w.wroteHeader)
}
