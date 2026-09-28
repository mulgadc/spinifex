package kvutil_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewBucketReport_TellsConfiguredFromWorking is the distinction the whole
// health report exists for. A stream can carry the replica count it was asked for
// while a peer is offline or still catching up, and that reads as healthy right up
// until the node actually serving it goes away.
func TestNewBucketReport_TellsConfiguredFromWorking(t *testing.T) {
	info := &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{Replicas: 3},
		Cluster: &jetstream.ClusterInfo{
			Name:   "spinifex",
			Leader: "spinifex-nats-node1",
			Replicas: []*jetstream.PeerInfo{
				{Name: "spinifex-nats-node2", Current: true},
				{Name: "spinifex-nats-node3", Current: false},
			},
		},
	}

	got := kvutil.NewBucketReportForTest("spinifex-instance-state", 3, info)

	assert.Equal(t, 3, got.Replicas, "the configured count is what the stream asked for")
	assert.Equal(t, 2, got.Online, "a peer that is not current cannot acknowledge a write")
	assert.Equal(t, "spinifex-nats-node1", got.Leader)
	assert.Equal(t, []string{"spinifex-nats-node1", "spinifex-nats-node2", "spinifex-nats-node3"}, got.Peers,
		"every assigned peer is named, leader first")
	assert.False(t, got.UnderReplicated())
	assert.True(t, got.HasQuorum(), "two of three is a majority")
	assert.True(t, got.Healthy())
}

// TestNewBucketReport_OfflinePeerIsNotOnline: JetStream reports current and offline
// separately, and a peer can be marked current from a stale view while being
// unreachable. Both have to disqualify it.
func TestNewBucketReport_OfflinePeerIsNotOnline(t *testing.T) {
	info := &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{Replicas: 3},
		Cluster: &jetstream.ClusterInfo{
			Leader: "spinifex-nats-node1",
			Replicas: []*jetstream.PeerInfo{
				{Name: "spinifex-nats-node2", Current: true, Offline: true},
				{Name: "spinifex-nats-node3", Current: true, Offline: true},
			},
		},
	}

	got := kvutil.NewBucketReportForTest("b", 3, info)

	assert.Equal(t, 1, got.Online)
	assert.False(t, got.HasQuorum(), "one of three cannot accept a write")
	assert.False(t, got.Healthy())
	assert.False(t, got.UnderReplicated(), "no quorum is not the same finding as too few replicas")
}

// TestNewBucketReport_NoLeaderIsNoQuorum: a raft group mid-election has no writer,
// and reporting it as serving would hide exactly the window this matters in.
func TestNewBucketReport_NoLeaderIsNoQuorum(t *testing.T) {
	info := &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{Replicas: 3},
		Cluster: &jetstream.ClusterInfo{
			Replicas: []*jetstream.PeerInfo{{Name: "spinifex-nats-node2", Current: true}},
		},
	}

	got := kvutil.NewBucketReportForTest("b", 3, info)

	assert.Empty(t, got.Leader)
	assert.Equal(t, 1, got.Online)
	assert.False(t, got.HasQuorum())
}

// TestNewBucketReport_SingleNodeServerReportsNoCluster is the single-node case, and
// it must not read as an outage. A non-clustered server sends no cluster block at
// all, and its one replica is serving — the read that produced this info was
// answered by it.
func TestNewBucketReport_SingleNodeServerReportsNoCluster(t *testing.T) {
	got := kvutil.NewBucketReportForTest("b", 1, &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{Replicas: 1},
	})

	assert.Equal(t, 1, got.Replicas)
	assert.Equal(t, 1, got.Online)
	assert.True(t, got.HasQuorum())
	assert.True(t, got.Healthy())
}

// TestHasQuorumIsMeasuredAgainstConfiguredReplicas: a bucket not yet raised is
// still able to serve at the size it has, and calling that a quorum failure would
// confuse a durability shortfall with an outage.
func TestHasQuorumIsMeasuredAgainstConfiguredReplicas(t *testing.T) {
	r := kvutil.BucketReport{Bucket: "b", Replicas: 1, Want: 3, Online: 1}
	assert.True(t, r.HasQuorum(), "one of one is a majority even when three are wanted")
	assert.True(t, r.UnderReplicated())
	assert.False(t, r.Healthy())
}

// TestRaiseThrottle_LetsTheFirstAttemptThroughThenWaits is what keeps a reconcile
// loop from billing a stream update and a warning line every few seconds while a
// cluster cannot place a replica. The first attempt is never delayed, because the
// throttle must not slow down a raise that would have succeeded.
func TestRaiseThrottle_LetsTheFirstAttemptThroughThenWaits(t *testing.T) {
	th := kvutil.NewRaiseThrottleForTest(time.Minute)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	require.True(t, th.Allow("alpha", start), "the first attempt must never be throttled")
	assert.False(t, th.Allow("alpha", start.Add(time.Second)), "a retry one second later is the noise this prevents")
	assert.False(t, th.Allow("alpha", start.Add(59*time.Second)))
	assert.True(t, th.Allow("alpha", start.Add(time.Minute)), "past the window it tries again, so a cluster that grows still converges")
}

// TestRaiseThrottle_IsPerBucket: one bucket that cannot be placed must not silence
// the raise for every other bucket on the node.
func TestRaiseThrottle_IsPerBucket(t *testing.T) {
	th := kvutil.NewRaiseThrottleForTest(time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	require.True(t, th.Allow("alpha", now))
	assert.True(t, th.Allow("beta", now), "beta has never been attempted")
	assert.False(t, th.Allow("alpha", now))
}
