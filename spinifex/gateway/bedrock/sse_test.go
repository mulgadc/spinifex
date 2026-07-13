package gateway_bedrock

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSEScanner_VLLMStyleBareDataLines(t *testing.T) {
	raw := "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n"
	sc := newSSEScanner(strings.NewReader(raw))

	ev1, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "", ev1.Event)
	assert.Equal(t, `{"a":1}`, ev1.Data)

	ev2, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"a":2}`, ev2.Data)

	ev3, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "[DONE]", ev3.Data)

	_, ok, err = sc.Next()
	assert.False(t, ok)
	assert.ErrorIs(t, err, io.EOF)
}

func TestSSEScanner_AnthropicStyleEventAndData(t *testing.T) {
	raw := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: ping\ndata: {}\n\n"
	sc := newSSEScanner(strings.NewReader(raw))

	ev1, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "message_start", ev1.Event)
	assert.Equal(t, `{"type":"message_start"}`, ev1.Data)

	ev2, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ping", ev2.Event)
}

func TestSSEScanner_MultiLineData(t *testing.T) {
	raw := "data: line one\ndata: line two\n\n"
	sc := newSSEScanner(strings.NewReader(raw))

	ev, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "line one\nline two", ev.Data)
}

func TestSSEScanner_SkipsCommentsAndKeepaliveBlankLines(t *testing.T) {
	raw := ":keepalive\n\n:another comment\ndata: {\"a\":1}\n\n"
	sc := newSSEScanner(strings.NewReader(raw))

	ev, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"a":1}`, ev.Data)
}

func TestSSEScanner_TrailingRecordWithoutFinalBlankLine(t *testing.T) {
	raw := "data: {\"a\":1}\n" // no trailing blank line before EOF
	sc := newSSEScanner(strings.NewReader(raw))

	ev, ok, err := sc.Next()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"a":1}`, ev.Data)

	_, ok, err = sc.Next()
	assert.False(t, ok)
	assert.ErrorIs(t, err, io.EOF)
}

func TestSSEScanner_EmptyStreamIsCleanEOF(t *testing.T) {
	sc := newSSEScanner(strings.NewReader(""))
	_, ok, err := sc.Next()
	assert.False(t, ok)
	assert.ErrorIs(t, err, io.EOF)
}

type errReader struct{ err error }

func (r errReader) Read(_ []byte) (int, error) { return 0, r.err }

func TestSSEScanner_ReadErrorPropagates(t *testing.T) {
	boom := assert.AnError
	sc := newSSEScanner(errReader{err: boom})
	_, ok, err := sc.Next()
	assert.False(t, ok)
	assert.ErrorIs(t, err, boom)
}
