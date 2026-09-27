package viperblockd

//test:in-package — both bucket constructors are unexported, and the replica
//count they are created with has no exported surface to read it back from.

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lease bucket decides which node may write a volume, and the dirty bucket
// is read on every mount. Created on one replica, both live on one node, and a
// cluster that loses that node can neither fence nor mount anywhere — which is
// the failure the whole recovery path is built to survive. So the cluster's node
// count has to reach NATS, and these assert that it does in both directions: a
// count the server cannot satisfy fails loudly rather than quietly creating one
// replica, and the cluster's count is the count the bucket is created with.
func TestVolumeBuckets_AreCreatedAtTheClusterReplicaCount(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	leases, err := newVolumeLeases(t.Context(), nc, "node-a")
	require.NoError(t, err)
	assert.Equal(t, 1, bucketReplicas(t, leases.kv))

	dirty, err := newVolumeDirty(t.Context(), nc, "node-a")
	require.NoError(t, err)
	assert.Equal(t, 1, bucketReplicas(t, dirty.kv))
}

// A single embedded server cannot hold three replicas, so declaring a
// three-node cluster is how a test proves the number is not dropped on the way.
func TestVolumeBuckets_RefuseAReplicaCountTheServerCannotHold(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	clustersize.Declare(3)
	t.Cleanup(func() { clustersize.Declare(1) })

	_, err = newVolumeLeases(t.Context(), nc, "node-a")
	require.Error(t, err, "a lease bucket that cannot be made quorate must fail the service, not serve unreplicated")

	_, err = newVolumeDirty(t.Context(), nc, "node-a")
	require.Error(t, err, "a dirty bucket that cannot be made quorate must fail the service, not serve unreplicated")
}

// TestVolumeBuckets_RefuseAnUndeclaredClusterSize pins the fail-closed choice at
// the call site that matters most: a process that never loaded a cluster config
// must not create these buckets on one node by default.
func TestVolumeBuckets_RefuseAnUndeclaredClusterSize(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	clustersize.Declare(0)
	t.Cleanup(func() { clustersize.Declare(1) })

	_, err = newVolumeLeases(t.Context(), nc, "node-a")
	require.Error(t, err)

	_, err = newVolumeDirty(t.Context(), nc, "node-a")
	require.Error(t, err)
}

func bucketReplicas(t *testing.T, kv jetstream.KeyValue) int {
	t.Helper()

	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	bucket, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok, "expected a JetStream-backed bucket")
	return bucket.StreamInfo().Config.Replicas
}
