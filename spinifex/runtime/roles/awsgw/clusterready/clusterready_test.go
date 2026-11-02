package clusterready

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
)

func TestAvailable(t *testing.T) {
	assert.False(t, New(nil).Available(), "no connection is never available")
	assert.False(t, New(&nats.Conn{}).Available(), "a connection that never connected is unavailable")
	var nilBus *Bus
	assert.False(t, nilBus.Available())
}
