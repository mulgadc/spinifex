package clusterv1_test

import (
	"encoding/json"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bucket and key are the deployed layout: a rolling upgrade has old daemons
// writing and new readers reading, so these strings cannot change.
func TestHeartbeatKey_IsTheDeployedLayout(t *testing.T) {
	assert.Equal(t, "spinifex-cluster-state", clusterv1.ClusterStateBucket)
	assert.Equal(t, "heartbeat.node-a", clusterv1.HeartbeatKey("node-a"))
}

func TestHeartbeatTimestamp_ReadsAFullDaemonHeartbeat(t *testing.T) {
	full := `{"node":"node-a","epoch":3,"timestamp":"2026-10-30T01:02:03Z","vm_count":4,"allocated_vcpu":8}`
	var h clusterv1.HeartbeatTimestamp
	require.NoError(t, json.Unmarshal([]byte(full), &h))

	written, ok := h.Time()
	require.True(t, ok)
	assert.True(t, written.Equal(time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)))
}

func TestHeartbeatTimestamp_UndatedBeatVouchesForNothing(t *testing.T) {
	for _, ts := range []string{"", "yesterday"} {
		_, ok := clusterv1.HeartbeatTimestamp{Timestamp: ts}.Time()
		assert.False(t, ok, "timestamp %q", ts)
	}
}

// A beat is live strictly under the window, so one exactly the window old is
// the first a takeover may treat as stopped.
func TestLiveAt_IsStrictlyUnderTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	assert.True(t, clusterv1.LiveAt(now.Add(-clusterv1.LivenessWindow+time.Nanosecond), now))
	assert.False(t, clusterv1.LiveAt(now.Add(-clusterv1.LivenessWindow), now))
	assert.True(t, clusterv1.LiveAt(now.Add(time.Second), now), "a beat from a clock running ahead is live")
}

func TestHeartbeatWritten_DatesAStoredBeatAndRefusesAnUndecodableOne(t *testing.T) {
	written, ok := clusterv1.HeartbeatWritten([]byte(`{"node":"node-a","timestamp":"2026-10-30T01:02:03Z"}`))
	require.True(t, ok)
	assert.True(t, written.Equal(time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)))

	for _, raw := range []string{"not json", `{"node":"node-a"}`} {
		_, ok := clusterv1.HeartbeatWritten([]byte(raw))
		assert.False(t, ok, raw)
	}
}
