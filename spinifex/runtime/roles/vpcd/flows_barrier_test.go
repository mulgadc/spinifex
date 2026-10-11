// Exercises newFlowsBarrier, which is unexported wiring, by swapping the
// unexported waitForFlowsHV var it closes over.
//
//test:in-package
package vpcd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The barrier has to confirm against the same NB cluster vpcd writes through.
// A barrier with no address falls back to the local unix socket, which on a
// node outside the configured cluster is a database that never saw the write.
func TestNewFlowsBarrierUsesTheConfiguredNBAddress(t *testing.T) {
	const nbAddr = "tcp:10.2.0.2:6641,tcp:10.2.0.3:6641,tcp:10.2.0.4:6641"

	original := waitForFlowsHV
	t.Cleanup(func() { waitForFlowsHV = original })

	var got string
	var calls int
	waitForFlowsHV = func(addr string) error {
		got = addr
		calls++
		return nil
	}

	require.NoError(t, newFlowsBarrier(nbAddr)())
	assert.Equal(t, 1, calls, "the barrier runs the sync once per call")
	assert.Equal(t, nbAddr, got, "the barrier confirms against the configured cluster")
}
