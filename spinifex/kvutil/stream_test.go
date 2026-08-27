package kvutil_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
)

func TestIsStreamUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a failure", nil, false},
		{"stream not found", jetstream.ErrStreamNotFound, true},
		{"no stream response", jetstream.ErrNoStreamResponse, true},
		{"no responders", nats.ErrNoResponders, true},
		{"wrapped sentinel", fmt.Errorf("load state: %w", jetstream.ErrStreamNotFound), true},
		{"untyped, matched on message", errors.New("nats: stream not found"), true},
		{"missing key is not a missing stream", jetstream.ErrKeyNotFound, false},
		{"unrelated failure", errors.New("some other error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kvutil.IsStreamUnavailable(tt.err))
		})
	}
}
