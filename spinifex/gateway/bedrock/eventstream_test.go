package gateway_bedrock

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	esv2 "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeFrame decodes one event-stream message off raw using the
// aws-sdk-go-v2 eventstream package's own Decoder, proving our encoded bytes
// round-trip through prelude/headers/CRC framing exactly as a real consumer
// would decode them.
func decodeFrame(t *testing.T, raw []byte) esv2.Message {
	t.Helper()
	dec := esv2.NewDecoder()
	msg, err := dec.Decode(bytes.NewReader(raw), nil)
	require.NoError(t, err)
	return msg
}

func headerString(t *testing.T, msg esv2.Message, name string) string {
	t.Helper()
	v := msg.Headers.Get(name)
	require.NotNilf(t, v, "header %q not present", name)
	return v.String()
}

func TestFrameWriter_WriteEvent_RoundTripsThroughDecoder(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	require.NoError(t, fw.writeEvent("messageStart", []byte(`{"role":"assistant"}`)))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, eventStreamContentType, rec.Header().Get("Content-Type"))

	msg := decodeFrame(t, rec.Body.Bytes())
	assert.Equal(t, "event", headerString(t, msg, ":message-type"))
	assert.Equal(t, "messageStart", headerString(t, msg, ":event-type"))
	assert.Equal(t, "application/json", headerString(t, msg, ":content-type"))
	assert.JSONEq(t, `{"role":"assistant"}`, string(msg.Payload))
}

func TestFrameWriter_WriteException_RoundTripsThroughDecoder(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	require.NoError(t, fw.writeException(excModelStreamErrorException, exceptionPayload(errors.New("upstream broke"))))

	msg := decodeFrame(t, rec.Body.Bytes())
	assert.Equal(t, "exception", headerString(t, msg, ":message-type"))
	assert.Equal(t, excModelStreamErrorException, headerString(t, msg, ":exception-type"))
	assert.JSONEq(t, `{"message":"upstream broke"}`, string(msg.Payload))
}

func TestFrameWriter_WriteChunk_Base64EncodesPayload(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	require.NoError(t, fw.writeChunk([]byte(`{"type":"content_block_delta"}`)))

	msg := decodeFrame(t, rec.Body.Bytes())
	assert.Equal(t, "chunk", headerString(t, msg, ":event-type"))
	assert.JSONEq(t, `{"bytes":"eyJ0eXBlIjoiY29udGVudF9ibG9ja19kZWx0YSJ9"}`, string(msg.Payload))
}

func TestFrameWriter_MultipleFrames_EachDecodesIndependently(t *testing.T) {
	rec := httptest.NewRecorder()
	fw, err := newFrameWriter(rec)
	require.NoError(t, err)

	require.NoError(t, fw.writeEvent("messageStart", []byte(`{"role":"assistant"}`)))
	require.NoError(t, fw.writeEvent("contentBlockDelta", []byte(`{"contentBlockIndex":0}`)))

	body := rec.Body.Bytes()
	dec := esv2.NewDecoder()
	r := bytes.NewReader(body)

	msg1, err := dec.Decode(r, nil)
	require.NoError(t, err)
	assert.Equal(t, "messageStart", headerString(t, msg1, ":event-type"))

	msg2, err := dec.Decode(r, nil)
	require.NoError(t, err)
	assert.Equal(t, "contentBlockDelta", headerString(t, msg2, ":event-type"))

	assert.Equal(t, 0, r.Len(), "decoder should have consumed the entire body across both frames")
}

// nonFlusherWriter implements only the bare http.ResponseWriter methods: no
// Flush, no Unwrap. It stands in for a hand-rolled ResponseWriter that
// cannot support streaming, proving newFrameWriter rejects it pre-header
// (Header/WriteHeader/Write are never called) rather than writing a 200 and
// then failing.
type nonFlusherWriter struct {
	header      http.Header
	wroteHeader bool
}

func (w *nonFlusherWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *nonFlusherWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *nonFlusherWriter) WriteHeader(int) { w.wroteHeader = true }

func TestNewFrameWriter_NonFlusherWriter_CleanPreHeaderError(t *testing.T) {
	w := &nonFlusherWriter{}
	fw, err := newFrameWriter(w)
	require.Error(t, err)
	assert.Nil(t, fw)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.False(t, w.wroteHeader, "newFrameWriter must not write a header when the writer can't support streaming")
}

func TestFlushSupported_WalksUnwrapChain(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapped := &unwrappingWriter{inner: rec}
	assert.True(t, flushSupported(wrapped))
}

// unwrappingWriter has no Flush of its own but exposes Unwrap, mirroring
// otelsetup's fixed statusRecorder: flushSupported must walk through it to
// find rec's Flush.
type unwrappingWriter struct {
	inner http.ResponseWriter
}

func (w *unwrappingWriter) Header() http.Header         { return w.inner.Header() }
func (w *unwrappingWriter) Write(b []byte) (int, error) { return w.inner.Write(b) }
func (w *unwrappingWriter) WriteHeader(code int)        { w.inner.WriteHeader(code) }
func (w *unwrappingWriter) Unwrap() http.ResponseWriter { return w.inner }
