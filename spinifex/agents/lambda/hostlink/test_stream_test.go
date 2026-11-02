//go:build lambdatestlink

package hostlink

import (
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pair returns the guest carrier and the raw host end of a pipe.
func pair(t *testing.T) (*TestStream, net.Conn) {
	t.Helper()
	g, h := net.Pipe()
	t.Cleanup(func() { g.Close(); h.Close() })
	return NewTestStream(g), h
}

func TestRoundTrip(t *testing.T) {
	guest, host := pair(t)
	hostEnd := NewTestStream(host)
	req := Request{GrantID: "g", AttemptID: "a", RequestID: "r", FunctionARN: "arn", TraceID: "t", DeadlineMs: 42, Payload: []byte{0, 1, 2, 255}}
	go func() { _ = hostEnd.HostSend(req) }()

	got, err := guest.Next(context.Background())
	require.NoError(t, err)
	assert.Equal(t, req, got)

	resp := Response{GrantID: "g", AttemptID: "a", RequestID: "r", Payload: []byte("out"), FunctionError: "Unhandled"}
	go func() { _ = guest.Complete(context.Background(), resp) }()
	gotResp, err := hostEnd.HostResult(context.Background())
	require.NoError(t, err)
	assert.Equal(t, resp, gotResp)
}

func TestOversizeFrameRefused(t *testing.T) {
	guest, host := pair(t)
	go func() {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], maxFrame+1)
		_, _ = host.Write(hdr[:])
	}()
	_, err := guest.Next(context.Background())
	require.ErrorIs(t, err, ErrFrameTooLarge)

	err = guest.Complete(context.Background(), Response{Payload: make([]byte, maxFrame)})
	require.ErrorIs(t, err, ErrFrameTooLarge)
}

func TestTruncatedFrameErrors(t *testing.T) {
	guest, host := pair(t)
	go func() {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 100)
		_, _ = host.Write(append(hdr[:], []byte(`{"type"`)...))
		host.Close()
	}()
	_, err := guest.Next(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrShutdown)
}

func TestShutdownSurfaced(t *testing.T) {
	guest, host := pair(t)
	hostEnd := NewTestStream(host)
	go func() { _ = hostEnd.HostShutdown() }()
	_, err := guest.Next(context.Background())
	require.ErrorIs(t, err, ErrShutdown)
}
