package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
)

// TestStartOchreVector_DisabledSkipsConstruction pins the config gate's
// default-off behavior (D-series Stage 5b): with OchreVector.Enabled false
// (the zero value, i.e. every daemon that has not opted in), startOchreVector
// must construct and subscribe nothing, leaving d.ochreVectorService nil and
// d.natsSubscriptions untouched. No JetStream/NATS connection, ctx or
// natsSubscriptions map is set on d here, so any attempt to construct or
// register past the Enabled check would panic (nil natsConn, nil ctx, or a
// nil-map write) -- the test passing at all is itself part of the pin.
func TestStartOchreVector_DisabledSkipsConstruction(t *testing.T) {
	d := &Daemon{
		config: &config.Config{},
	}

	d.startOchreVector()

	assert.Nil(t, d.ochreVectorService)
	assert.Nil(t, d.natsSubscriptions, "disabled path must not touch the subscriptions map")
}
