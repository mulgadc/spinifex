package vbwire_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/services/viperblockd/vbwire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the deployed wire shapes of vbwire before any move to
// contracts/viperblockd/legacy/v1, so the move can be checked byte-for-byte
// against these literals rather than against the package's own definitions.

// TestVolumeAbandonSubject_BothForms pins the abandon route the same way the
// fenced route is pinned in vbwire_test.go: node-addressed, with an
// empty-node fallback for a single-node daemon.
func TestVolumeAbandonSubject_BothForms(t *testing.T) {
	assert.Equal(t, "ebs.node-a.abandon", vbwire.VolumeAbandonSubject("node-a"))
	assert.Equal(t, "ebs.abandon", vbwire.VolumeAbandonSubject(""))
}

// TestDirtyBucket_Name pins the JetStream KV bucket name. Renaming it without
// a migration would strand every existing dirty marker in an unread bucket.
func TestDirtyBucket_Name(t *testing.T) {
	assert.Equal(t, "VIPERBLOCK_VOLUME_DIRTY", vbwire.DirtyBucket)
}

// TestVolumeFencedEvent_JSON pins the exact field spelling of the fenced
// event, published by viperblockd and consumed by the daemon.
func TestVolumeFencedEvent_JSON(t *testing.T) {
	golden := `{"volume":"vol-1","node":"node-a","winner":"node-b","reason":"volume lease moved to node-b while this node had the volume open"}`
	event := vbwire.VolumeFencedEvent{
		Volume: "vol-1",
		Node:   "node-a",
		Winner: "node-b",
		Reason: "volume lease moved to node-b while this node had the volume open",
	}

	marshaled, err := json.Marshal(event)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded vbwire.VolumeFencedEvent
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, event, decoded)
}

// TestVolumeAbandonRequest_JSON pins the request daemon/vm_adapters.go sends
// on the abandon route.
func TestVolumeAbandonRequest_JSON(t *testing.T) {
	golden := `{"volume":"vol-1","reason":"instance owned by another node"}`
	req := vbwire.VolumeAbandonRequest{Volume: "vol-1", Reason: "instance owned by another node"}

	marshaled, err := json.Marshal(req)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded vbwire.VolumeAbandonRequest
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, req, decoded)
}

// TestVolumeAbandonResponse_JSON pins both response shapes: the ordinary
// reconciling case with no error, and the failure case where Error is set.
// Error is omitempty, so the two must not be mistaken for the same shape.
func TestVolumeAbandonResponse_JSON(t *testing.T) {
	t.Run("abandoned, no error", func(t *testing.T) {
		golden := `{"abandoned":true}`
		resp := vbwire.VolumeAbandonResponse{Abandoned: true}

		marshaled, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))

		var decoded vbwire.VolumeAbandonResponse
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, resp, decoded)
	})

	t.Run("not abandoned, with error", func(t *testing.T) {
		golden := `{"abandoned":false,"error":"nbdkit did not exit"}`
		resp := vbwire.VolumeAbandonResponse{Abandoned: false, Error: "nbdkit did not exit"}

		marshaled, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.JSONEq(t, golden, string(marshaled))

		var decoded vbwire.VolumeAbandonResponse
		require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
		assert.Equal(t, resp, decoded)
	})
}

// TestDirtyRecord_JSON pins the stored JSON shape read across nodes from the
// dirty bucket, including the RFC 3339 encoding of Since.
func TestDirtyRecord_JSON(t *testing.T) {
	since := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	golden := `{"owner":"node-a","generation":7,"since":"2024-01-02T03:04:05Z","reason":"seal volume: predastore unreachable"}`
	record := vbwire.DirtyRecord{
		Owner:      "node-a",
		Generation: 7,
		Since:      since,
		Reason:     "seal volume: predastore unreachable",
	}

	marshaled, err := json.Marshal(record)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(marshaled))

	var decoded vbwire.DirtyRecord
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.True(t, decoded.Since.Equal(since))
	decoded.Since = since
	assert.Equal(t, record, decoded)
}
