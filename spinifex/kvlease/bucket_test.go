package kvlease_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/kvlease"
	"github.com/mulgadc/spinifex/spinifex/testutil"
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
	clustersize.Declare(3)
	t.Cleanup(func() { clustersize.Declare(1) })

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

	clustersize.Declare(0)
	t.Cleanup(func() { clustersize.Declare(1) })

	_, err := kvlease.OpenBucket(t.Context(), js, kvlease.BucketConfig{Name: "lease-undeclared", TTL: time.Minute})
	require.ErrorIs(t, err, clustersize.ErrUndeclared)
}
