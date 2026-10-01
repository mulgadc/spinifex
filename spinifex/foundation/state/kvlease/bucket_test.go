package kvlease_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvlease"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenBucket_OpensALeaseBucketTheClusterCannotYetRaise is the assertion that
// keeps this fix from becoming the outage it prevents.
//
// OpenBucket runs on every reconcile tick. If a raise it cannot place failed the
// open, no reconciler could acquire its lease, and every one of them would stop
// cluster-wide — over a lease bucket that is present and quorate at the replica
// count it already has. The config naming a node that is not serving yet is the
// documented way a cluster grows, so this is a state to tolerate, not to refuse.
func TestOpenBucket_OpensALeaseBucketTheClusterCannotYetRaise(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	defer nc.Close()

	cfg := kvlease.BucketConfig{Name: "lease-growing-cluster", TTL: time.Minute}
	kv, err := kvlease.OpenBucket(t.Context(), js, cfg)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "leader", []byte("node1"))
	require.NoError(t, err)

	// One embedded server cannot hold three replicas, so the raise must fail.
	clustersize.RedeclareForTest(t, 3)

	reopened, err := kvlease.OpenBucket(t.Context(), js, cfg)
	require.NoError(t, err, "a reconciler must still be able to reach its lease bucket")
	entry, err := reopened.Get(t.Context(), "leader")
	require.NoError(t, err)
	assert.Equal(t, "node1", string(entry.Value()))
}

// TestOpenBucket_RefusesWhenClusterSizeIsUndeclared pins the fail-closed half:
// creating a lease bucket without knowing the cluster size must not guess at one
// replica, which is the state that makes a lease unacquirable when a node fails.
func TestOpenBucket_RefusesWhenClusterSizeIsUndeclared(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	defer nc.Close()

	clustersize.RedeclareForTest(t, 0)

	_, err := kvlease.OpenBucket(t.Context(), js, kvlease.BucketConfig{Name: "lease-undeclared", TTL: time.Minute})
	require.ErrorIs(t, err, clustersize.ErrUndeclared)
}

// TestNATSBucket_StopsOnAConfigFault covers the retry loop rather than the open.
//
// The loop exists because a cold multi-node start has no JetStream quorum yet, and
// waiting for one is right. An undeclared cluster size never becomes declared by
// waiting, so a loop that cannot tell them apart holds a reconciler for its whole
// retry window and then reports the bucket as unreachable — which was never the
// problem.
func TestNATSBucket_StopsOnAConfigFault(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	defer nc.Close()

	clustersize.RedeclareForTest(t, 0)

	start := time.Now()
	_, err := kvlease.NATSBucket(nc, "lease-config-fault", time.Minute)(t.Context())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, clustersize.ErrUndeclared,
		"the error has to name the config fault, not the bucket it never reached")
	require.Lessf(t, elapsed, 10*time.Second,
		"returned after %s; a permanent fault must not consume the retry window", elapsed)
}
