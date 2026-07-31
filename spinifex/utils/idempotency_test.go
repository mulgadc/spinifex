package utils

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdempotencyKeyFromContext(t *testing.T) {
	assert.Empty(t, IdempotencyKeyFromContext(context.Background()))
	assert.Equal(t, "abc", IdempotencyKeyFromContext(WithIdempotencyKey(context.Background(), "abc")))
}

func TestIdempotencyKeyFromMsg(t *testing.T) {
	assert.Empty(t, IdempotencyKeyFromMsg(nil))
	assert.Empty(t, IdempotencyKeyFromMsg(&nats.Msg{}))

	msg := &nats.Msg{Header: nats.Header{}}
	msg.Header.Set(IdempotencyKeyHeader, "abc")
	assert.Equal(t, "abc", IdempotencyKeyFromMsg(msg))
}

// The token is set on the HTTP request and consumed by the service behind NATS,
// so it has to survive the hop or the dedupe never sees it.
func TestNATSRequest_ForwardsIdempotencyKey(t *testing.T) {
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	type resp struct {
		Key string `json:"key"`
	}
	_, err = nc.Subscribe("test.idem", func(msg *nats.Msg) {
		data, _ := json.Marshal(resp{Key: IdempotencyKeyFromMsg(msg)})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)

	ctx := WithIdempotencyKey(context.Background(), "invocation-1")
	got, err := NATSRequest[resp](ctx, nc, "test.idem", struct{}{}, 2*time.Second, "111122223333")
	require.NoError(t, err)
	assert.Equal(t, "invocation-1", got.Key)

	// No key in context must leave the header unset rather than send an empty one.
	got, err = NATSRequest[resp](context.Background(), nc, "test.idem", struct{}{}, 2*time.Second, "111122223333")
	require.NoError(t, err)
	assert.Empty(t, got.Key)
}
