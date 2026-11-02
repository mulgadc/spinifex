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
