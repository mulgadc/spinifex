//test:in-package — clusterHeartbeats is an unexported daemon adapter
package daemon

import (
	"context"
	"encoding/json"
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
	h := clusterHeartbeats{kv: kv, js: js}
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

	_, _, err = clusterHeartbeats{kv: kv, js: js}.LastSeen(t.Context(), "node-a")
	require.Error(t, err)
}

// laggingReplica answers every get as a replica behind the leader would:
// with the beat it held before, or with no key at all.
type laggingReplica struct {
	jetstream.KeyValue

	old []byte
}

func (r laggingReplica) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if r.old == nil {
		return nil, jetstream.ErrKeyNotFound
	}
	return staleEntry{value: r.old}, nil
}

type staleEntry struct {
	jetstream.KeyValueEntry

	value []byte
}

func (e staleEntry) Value() []byte { return e.value }

// TestS7_HeartbeatALaggingReplicaMissesIsReadFromTheLeader enforces
// ADR-0003:S7: a get a lagging replica answers with an old beat, or none, must
// not read as a dead holder while the leader holds a fresh one.
func TestS7_HeartbeatALaggingReplicaMissesIsReadFromTheLeader(t *testing.T) {
	old, err := json.Marshal(clusterv1.HeartbeatTimestamp{Timestamp: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)})
	require.NoError(t, err)
	for name, replica := range map[string][]byte{"an old beat": old, "no key": nil} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: clusterv1.ClusterStateBucket})
			require.NoError(t, err)
			fresh := time.Now().UTC().Truncate(time.Second)
			body, err := json.Marshal(clusterv1.HeartbeatTimestamp{Timestamp: fresh.Format(time.RFC3339)})
			require.NoError(t, err)
			_, err = kv.Put(t.Context(), clusterv1.HeartbeatKey("node-b"), body)
			require.NoError(t, err)

			seen, found, err := clusterHeartbeats{kv: laggingReplica{KeyValue: kv, old: replica}, js: js}.LastSeen(t.Context(), "node-b")

			require.NoError(t, err)
			require.True(t, found, "ADR-0003:S7 (uefi_store_claims.go LastSeen): a lagging replica's answer was trusted; confirm a stale or absent beat with the stream leader")
			assert.True(t, seen.Equal(fresh), "ADR-0003:S7: the leader's beat must be the one returned")
			assert.True(t, clusterv1.LiveAt(seen, time.Now()))
		})
	}
}

// A deleted heartbeat is no heartbeat, as an absent key is.
func TestClusterHeartbeats_DeletedBeatIsNotFound(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: clusterv1.ClusterStateBucket})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), clusterv1.HeartbeatKey("node-b"), []byte(`{"timestamp":"2026-11-05T01:02:03Z"}`))
	require.NoError(t, err)
	require.NoError(t, kv.Delete(t.Context(), clusterv1.HeartbeatKey("node-b")))

	_, found, err := clusterHeartbeats{kv: kv, js: js}.LastSeen(t.Context(), "node-b")
	require.NoError(t, err)
	assert.False(t, found)
}
