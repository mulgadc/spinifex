package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
)

// TestStartOchreVector_DisabledSkipsConstruction pins the config gate's
// default-off behavior (D-series Stage 5b): with OchreVector.Enabled false
// (the zero value, i.e. every daemon that has not opted in), startOchreVector
// must construct nothing and leave d.ochreVectorService nil, so subscribeAll
// registers no ochre.vector.* subject. No JetStream/NATS connection is set on
// d here, so any attempt to construct past the Enabled check would panic on
// a nil natsConn -- the test passing at all is itself part of the pin.
func TestStartOchreVector_DisabledSkipsConstruction(t *testing.T) {
	d := &Daemon{
		config: &config.Config{},
	}

	d.startOchreVector()

	assert.Nil(t, d.ochreVectorService)
}
