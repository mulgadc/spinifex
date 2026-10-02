package idempotency

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
)

func TestKeyFromContext(t *testing.T) {
	assert.Empty(t, KeyFromContext(context.Background()))
	assert.Equal(t, "abc", KeyFromContext(WithKey(context.Background(), "abc")))
}

func TestKeyFromMsg(t *testing.T) {
	assert.Empty(t, KeyFromMsg(nil))
	assert.Empty(t, KeyFromMsg(&nats.Msg{}))

	msg := &nats.Msg{Header: nats.Header{}}
	msg.Header.Set(KeyHeader, "abc")
	assert.Equal(t, "abc", KeyFromMsg(msg))
}
