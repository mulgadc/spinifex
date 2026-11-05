//test:in-package — clusterHeartbeats is an unexported daemon adapter
package daemon

import (
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The claim's takeover rule reads the same heartbeat the daemon writes. Only
// an absent beat is not found: one that exists but cannot be dated may be a
// live node's, so it is an error the claim refuses on (ADR-0003:S7).
func TestClusterHeartbeats_LastSeen(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: clusterv1.ClusterStateBucket})
	require.NoError(t, err)
	h := clusterHeartbeats{kv: kv}
	_, err = kv.Put(t.Context(), clusterv1.HeartbeatKey("node-a"), []byte(`{"node":"node-a","timestamp":"2026-11-05T01:02:03Z"}`))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), clusterv1.HeartbeatKey("node-b"), []byte(`not json`))
	require.NoError(t, err)

	seen, found, err := h.LastSeen(t.Context(), "node-a")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, seen.Equal(time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)))

	_, found, err = h.LastSeen(t.Context(), "node-b")
	require.Error(t, err, "ADR-0003:S7 (uefi_store_claims.go LastSeen): an undecodable heartbeat must not read as absent, or a live holder is taken over")
	assert.False(t, found)

	_, found, err = h.LastSeen(t.Context(), "node-c")
	require.NoError(t, err)
	assert.False(t, found)
}

// A bucket that cannot be read is an error, which the claim reads as a
// holder that may be live.
func TestClusterHeartbeats_UnreadableBucketIsAnError(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: clusterv1.ClusterStateBucket})
	require.NoError(t, err)
	require.NoError(t, js.DeleteKeyValue(t.Context(), clusterv1.ClusterStateBucket))

	_, _, err = clusterHeartbeats{kv: kv}.LastSeen(t.Context(), "node-a")
	require.Error(t, err)
}
