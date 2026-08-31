//test:in-package the barrier's staleness rules are only testable from inside:
// they need the clock seam, the freshness constants, the shared JetStream test
// server, and jsManager.clusterKV to seed heartbeats against.

package daemon

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestIPSecBarrier(t *testing.T) (*KVIPSecBarrier, *JetStreamManager) {
	t.Helper()

	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitClusterStateBucket())

	barrier := NewKVIPSecBarrier(jsm.clusterKV)
	require.NotNil(t, barrier)
	return barrier, jsm
}

// beat marks a node live without saying anything about its IPsec state.
func beat(t *testing.T, jsm *JetStreamManager, node string) {
	t.Helper()
	require.NoError(t, jsm.WriteHeartbeat(&Heartbeat{
		Node:      node,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}))
}

func TestKVIPSecBarrier_ReportsEveryLiveNodesState(t *testing.T) {
	barrier, jsm := newTestIPSecBarrier(t)
	nodes := []string{"ipsec-a", "ipsec-b"}
	for _, n := range nodes {
		beat(t, jsm, n)
	}

	require.NoError(t, barrier.Publish(t.Context(), "ipsec-a", host.IPSecNodeStatus{Ready: true, NBReachable: true}))

	cluster, err := barrier.Cluster(t.Context(), nodes)
	require.NoError(t, err)
	assert.Equal(t, map[string]host.IPSecNodeStatus{
		"ipsec-a": {Ready: true, NBReachable: true},
		"ipsec-b": {},
	}, cluster)

	require.NoError(t, barrier.Publish(t.Context(), "ipsec-b", host.IPSecNodeStatus{Ready: true}))

	cluster, err = barrier.Cluster(t.Context(), nodes)
	require.NoError(t, err)
	assert.Equal(t, map[string]host.IPSecNodeStatus{
		"ipsec-a": {Ready: true, NBReachable: true},
		"ipsec-b": {Ready: true},
	}, cluster)
}

// A node that publishes Ready=false has said its own setup is incomplete, which
// is exactly the case the flag must not be asserted over.
func TestKVIPSecBarrier_ExplicitlyUnreadyNodeIsReported(t *testing.T) {
	barrier, jsm := newTestIPSecBarrier(t)
	beat(t, jsm, "ipsec-unready")

	require.NoError(t, barrier.Publish(t.Context(), "ipsec-unready", host.IPSecNodeStatus{}))

	cluster, err := barrier.Cluster(t.Context(), []string{"ipsec-unready"})
	require.NoError(t, err)
	assert.Equal(t, map[string]host.IPSecNodeStatus{"ipsec-unready": {}}, cluster)
}

// A node taken out of service has no chassis registered, so there is no tunnel
// to it to black-hole. Reporting it as unconfigured would strip encryption from
// the peers that are still talking to each other.
func TestKVIPSecBarrier_NodeThatStoppedHeartbeatingDropsOutOfTheSet(t *testing.T) {
	barrier, jsm := newTestIPSecBarrier(t)
	beat(t, jsm, "ipsec-gone")

	require.NoError(t, barrier.Publish(t.Context(), "ipsec-gone", host.IPSecNodeStatus{Ready: true}))

	// Far enough ahead that both the status record and the heartbeat are stale.
	barrier.now = func() time.Time { return time.Now().Add(ipsecStatusFreshness + time.Hour) }

	cluster, err := barrier.Cluster(t.Context(), []string{"ipsec-gone"})
	require.NoError(t, err)
	assert.Empty(t, cluster)
}

// Still heartbeating but its status has gone stale: the node is up and can no
// longer vouch for its own configuration, so it counts as not ready.
func TestKVIPSecBarrier_LiveNodeWithAStaleStatusIsNotReady(t *testing.T) {
	barrier, jsm := newTestIPSecBarrier(t)

	require.NoError(t, barrier.Publish(t.Context(), "ipsec-stale", host.IPSecNodeStatus{Ready: true}))

	// The status is stale, but a heartbeat lands at the shifted clock.
	shifted := time.Now().Add(ipsecStatusFreshness + time.Minute)
	barrier.now = func() time.Time { return shifted }
	require.NoError(t, jsm.WriteHeartbeat(&Heartbeat{
		Node:      "ipsec-stale",
		Timestamp: shifted.UTC().Format(time.RFC3339),
	}))

	cluster, err := barrier.Cluster(t.Context(), []string{"ipsec-stale"})
	require.NoError(t, err)
	assert.Equal(t, map[string]host.IPSecNodeStatus{"ipsec-stale": {}}, cluster)
}

// A node that has never published anything and never heartbeated is not part of
// the running cluster yet.
func TestKVIPSecBarrier_UnknownNodeIsAbsent(t *testing.T) {
	barrier, _ := newTestIPSecBarrier(t)

	cluster, err := barrier.Cluster(t.Context(), []string{"ipsec-never-seen"})
	require.NoError(t, err)
	assert.Empty(t, cluster)
}

func TestKVIPSecBarrier_PublishRequiresANodeName(t *testing.T) {
	barrier, _ := newTestIPSecBarrier(t)

	err := barrier.Publish(t.Context(), "", host.IPSecNodeStatus{Ready: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node name unset")
}

// A nil KV means no cluster to be out of step with, and the caller relies on a
// nil interface rather than a typed nil to tell.
func TestNewKVIPSecBarrier_NilKVYieldsNoBarrier(t *testing.T) {
	assert.Nil(t, NewKVIPSecBarrier(nil))
}

// assert.Nil passes for a typed nil, so the interface conversion is checked with
// == instead: a non-nil interface holding a nil pointer panics in host.
func TestDaemonIPSecBarrier_NilManagerYieldsANilInterface(t *testing.T) {
	d := &Daemon{}
	assert.Equal(t, host.IPSecBarrier(nil), d.ipsecBarrier())
}

// The heartbeat that keeps a node in the set must be refreshed far more often
// than it is allowed to age, or a healthy node drops out between beats.
func TestIPSecLivenessWindowExceedsHeartbeatInterval(t *testing.T) {
	assert.Greater(t, ipsecLivenessWindow, 3*heartbeatInterval)
}
