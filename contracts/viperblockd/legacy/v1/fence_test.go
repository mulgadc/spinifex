package viperblocklegacyv1_test

import (
	"encoding/json"
	"testing"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVolumeFencedSubject_IsNodeAddressed pins the routing. A fenced guest is on
// the node that lost the volume, so a subject without the node in it would reach
// daemons with nothing to stop and, on a queue group, miss the one that has.
func TestVolumeFencedSubject_IsNodeAddressed(t *testing.T) {
	assert.Equal(t, "ebs.node-a.fenced", viperblocklegacyv1.VolumeFencedSubject("node-a"))
	assert.Equal(t, "ebs.fenced", viperblocklegacyv1.VolumeFencedSubject(""),
		"a single-node daemon has no node name, and still has to hear its own fences")
}

// TestVolumeFencedEvent_JSON pins the exact field spelling of the fenced
// event, published by viperblockd and consumed by the daemon.
func TestVolumeFencedEvent_JSON(t *testing.T) {
	golden := `{"volume":"vol-1","node":"node-a","winner":"node-b","reason":"volume lease moved to node-b while this node had the volume open"}`
	event := viperblocklegacyv1.VolumeFencedEvent{
		Volume: "vol-1",
		Node:   "node-a",
		Winner: "node-b",
		Reason: "volume lease moved to node-b while this node had the volume open",
	}

	marshaled, err := json.Marshal(event)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded viperblocklegacyv1.VolumeFencedEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, event, decoded)
}
