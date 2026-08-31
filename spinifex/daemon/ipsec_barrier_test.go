package daemon

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestIPSecBarrier(t *testing.T) *KVIPSecBarrier {
	t.Helper()

	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitClusterStateBucket())

	barrier := NewKVIPSecBarrier(jsm.clusterKV)
	require.NotNil(t, barrier)
	return barrier
}

func TestKVIPSecBarrier_ReadyOnlyWhenEveryNodeHasPublished(t *testing.T) {
	barrier := newTestIPSecBarrier(t)
	nodes := []string{"ipsec-a", "ipsec-b"}

	require.NoError(t, barrier.PublishLocalReady(t.Context(), "ipsec-a", true))

	ready, pending, err := barrier.NodesReady(t.Context(), nodes)
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Equal(t, []string{"ipsec-b"}, pending)

	require.NoError(t, barrier.PublishLocalReady(t.Context(), "ipsec-b", true))

	ready, pending, err = barrier.NodesReady(t.Context(), nodes)
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Empty(t, pending)
}

// A node that publishes false has said its own setup is incomplete, which is
// exactly the case the flag must not be asserted over.
func TestKVIPSecBarrier_UnreadyNodeIsPending(t *testing.T) {
	barrier := newTestIPSecBarrier(t)

	require.NoError(t, barrier.PublishLocalReady(t.Context(), "ipsec-unready", false))

	ready, pending, err := barrier.NodesReady(t.Context(), []string{"ipsec-unready"})
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Equal(t, []string{"ipsec-unready"}, pending)
}

// A record that has stopped being refreshed is not evidence the node is still
// configured; treating it as such would hold encryption over a chassis that
// came back up with nothing set.
func TestKVIPSecBarrier_StaleRecordIsPending(t *testing.T) {
	barrier := newTestIPSecBarrier(t)

	require.NoError(t, barrier.PublishLocalReady(t.Context(), "ipsec-stale", true))

	barrier.now = func() time.Time { return time.Now().Add(ipsecReadyFreshness + time.Minute) }

	ready, pending, err := barrier.NodesReady(t.Context(), []string{"ipsec-stale"})
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Equal(t, []string{"ipsec-stale"}, pending)
}

func TestKVIPSecBarrier_PublishRequiresANodeName(t *testing.T) {
	barrier := newTestIPSecBarrier(t)

	err := barrier.PublishLocalReady(t.Context(), "", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node name unset")
}

// A nil KV means no cluster to be out of step with, and the caller relies on a
// nil interface rather than a typed nil to tell.
func TestNewKVIPSecBarrier_NilKVYieldsNoBarrier(t *testing.T) {
	assert.Nil(t, NewKVIPSecBarrier(nil))
}
