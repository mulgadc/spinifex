package viperblocklegacyv1_test

import (
	"encoding/json"
	"testing"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVolumeAbandonSubject_BothForms pins the abandon route the same way the
// fenced route is pinned: node-addressed, with an empty-node fallback for a
// single-node daemon.
func TestVolumeAbandonSubject_BothForms(t *testing.T) {
	assert.Equal(t, "ebs.node-a.abandon", viperblocklegacyv1.VolumeAbandonSubject("node-a"))
	assert.Equal(t, "ebs.abandon", viperblocklegacyv1.VolumeAbandonSubject(""))
}

// TestVolumeAbandonRequest_JSON pins the request daemon/vm_adapters.go sends
// on the abandon route.
func TestVolumeAbandonRequest_JSON(t *testing.T) {
	golden := `{"volume":"vol-1","reason":"instance owned by another node"}`
	req := viperblocklegacyv1.VolumeAbandonRequest{Volume: "vol-1", Reason: "instance owned by another node"}

	marshaled, err := json.Marshal(req)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded viperblocklegacyv1.VolumeAbandonRequest
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, req, decoded)
}

// TestVolumeAbandonResponse_JSON pins both response shapes: the ordinary
// reconciling case with no error, and the failure case where Error is set.
// Error is omitempty, so the two must not be mistaken for the same shape.
func TestVolumeAbandonResponse_JSON(t *testing.T) {
	t.Run("abandoned, no error", func(t *testing.T) {
		golden := `{"abandoned":true}`
		resp := viperblocklegacyv1.VolumeAbandonResponse{Abandoned: true}

		marshaled, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))

		var decoded viperblocklegacyv1.VolumeAbandonResponse
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, resp, decoded)
	})

	t.Run("not abandoned, with error", func(t *testing.T) {
		golden := `{"abandoned":false,"error":"nbdkit did not exit"}`
		resp := viperblocklegacyv1.VolumeAbandonResponse{Abandoned: false, Error: "nbdkit did not exit"}

		marshaled, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))

		var decoded viperblocklegacyv1.VolumeAbandonResponse
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, resp, decoded)
	})
}
