package vbwire

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVolumeFencedSubject_IsNodeAddressed pins the routing. A fenced guest is on
// the node that lost the volume, so a subject without the node in it would reach
// daemons with nothing to stop and, on a queue group, miss the one that has.
func TestVolumeFencedSubject_IsNodeAddressed(t *testing.T) {
	assert.Equal(t, "ebs.node-a.fenced", VolumeFencedSubject("node-a"))
	assert.Equal(t, "ebs.fenced", VolumeFencedSubject(""),
		"a single-node daemon has no node name, and still has to hear its own fences")
}
