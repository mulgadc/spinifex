package kvutil_test

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditBucketReplicas_ReportsEveryBucketAgainstTheClusterSize(t *testing.T) {
	js := newJetStream(t)

	for _, bucket := range []string{"audit-alpha", "audit-beta"} {
		_, err := kvutil.GetOrCreateBucket(t.Context(), js, bucket, 1)
		require.NoError(t, err)
	}

	reports, err := kvutil.AuditBucketReplicas(t.Context(), js)
	require.NoError(t, err)
	require.Len(t, reports, 2)
	assert.Equal(t, "audit-alpha", reports[0].Bucket, "reports are sorted by bucket name")

	for _, r := range reports {
		assert.Equal(t, 1, r.Replicas)
		assert.Equal(t, 1, r.Want)
		assert.False(t, r.UnderReplicated())
	}

	// The cluster grew. Every existing bucket is now behind it, which is exactly
	// what the audit has to say before anything can repair it.
	clustersize.RedeclareForTest(t, 3)

	reports, err = kvutil.AuditBucketReplicas(t.Context(), js)
	require.NoError(t, err)
	require.Len(t, reports, 2)
	for _, r := range reports {
		assert.Equal(t, 1, r.Replicas)
		assert.Equal(t, 3, r.Want)
		assert.True(t, r.UnderReplicated(), "%s", r.Bucket)
	}
}

func TestAuditBucketReplicas_RefusesWhenClusterSizeIsUndeclared(t *testing.T) {
	js := newJetStream(t)
	clustersize.RedeclareForTest(t, 0)

	_, err := kvutil.AuditBucketReplicas(t.Context(), js)
	require.ErrorIs(t, err, clustersize.ErrUndeclared)
}

// TestRaiseBucketReplicas_NeverLowers is the property that makes the raise safe
// to run on every service start: a bucket already at or above the count is left
// alone, so a cluster that has lost a node does not have its durability cut to
// match.
func TestRaiseBucketReplicas_NeverLowers(t *testing.T) {
	js := newJetStream(t)

	_, err := kvutil.GetOrCreateBucket(t.Context(), js, "no-downgrade", 1)
	require.NoError(t, err)

	require.NoError(t, kvutil.RaiseBucketReplicas(t.Context(), js, "no-downgrade", 1))
	assert.Equal(t, 1, streamConfig(t, js, "no-downgrade").Replicas)

	// Zero and negative are below the current count, so they are no-ops too
	// rather than an update that would ask the server for an invalid config.
	require.NoError(t, kvutil.RaiseBucketReplicas(t.Context(), js, "no-downgrade", 0))
	assert.Equal(t, 1, streamConfig(t, js, "no-downgrade").Replicas)
}

// TestRaiseBucketReplicas_PreservesTheRestOfTheConfig is why this updates the
// stream rather than the KV bucket: a KeyValueConfig cannot express a stream's
// full configuration, so applying one would reset whatever it omits.
func TestRaiseBucketReplicas_PreservesTheRestOfTheConfig(t *testing.T) {
	js := newJetStream(t)

	_, err := kvutil.GetOrCreateBucketWithOptions(t.Context(), js, kvutil.BucketOptions{
		Name:        "config-preserved",
		Description: "a bucket with settings worth not losing",
		History:     7,
	})
	require.NoError(t, err)

	before := streamConfig(t, js, "config-preserved")
	require.NoError(t, kvutil.RaiseBucketReplicas(t.Context(), js, "config-preserved", 1))
	after := streamConfig(t, js, "config-preserved")

	assert.Equal(t, before.Description, after.Description)
	assert.Equal(t, before.MaxMsgsPerSubject, after.MaxMsgsPerSubject, "history is MaxMsgsPerSubject on the stream")
	assert.Equal(t, before.MaxAge, after.MaxAge)
}

// TestRaiseBucketReplicas_SurfacesAMissingBucket keeps a raise against a bucket
// that is not there from reading as success.
func TestRaiseBucketReplicas_SurfacesAMissingBucket(t *testing.T) {
	js := newJetStream(t)

	err := kvutil.RaiseBucketReplicas(t.Context(), js, "never-created", 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, jetstream.ErrStreamNotFound)
}

func streamConfig(t *testing.T, js jetstream.JetStream, bucket string) jetstream.StreamConfig {
	t.Helper()
	stream, err := js.Stream(t.Context(), "KV_"+bucket)
	require.NoError(t, err)
	info, err := stream.Info(t.Context())
	require.NoError(t, err)
	return info.Config
}

// newJetStream starts an embedded one-node JetStream for a test in this package.
func newJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return testutil.NewJetStream(t, nc)
}

// TestRaiseAllBucketReplicas_RaisesWhatItCanAndReportsWhatItCannot covers the
// sweep's two halves at once. It is the cluster-wide counterpart to the raise a
// bucket gets when a service opens it: a node joining changes the answer for
// every bucket, and most will not be reopened until something restarts.
func TestRaiseAllBucketReplicas_RaisesWhatItCanAndReportsWhatItCannot(t *testing.T) {
	js := newJetStream(t)

	for _, b := range []string{"sweep-alpha", "sweep-beta"} {
		_, err := kvutil.GetOrCreateBucket(t.Context(), js, b, 1)
		require.NoError(t, err)
	}

	// Nothing is short at one node, so the sweep must change nothing and say so
	// rather than rewriting every bucket it looked at.
	raised, err := kvutil.RaiseAllBucketReplicas(t.Context(), js)
	require.NoError(t, err)
	assert.Zero(t, raised, "a healthy cluster needs no repair")

	// The cluster grew beyond what one embedded server can place, so every
	// bucket is short and every raise fails. The sweep must attempt all of them
	// and join the failures rather than stopping at the first.
	clustersize.RedeclareForTest(t, 3)

	raised, err = kvutil.RaiseAllBucketReplicas(t.Context(), js)
	require.Error(t, err)
	assert.Zero(t, raised)
	assert.Contains(t, err.Error(), "sweep-alpha")
	assert.Contains(t, err.Error(), "sweep-beta", "the sweep stopped at the first failure")
	assert.Equal(t, 1, streamConfig(t, js, "sweep-alpha").Replicas, "a failed raise must not have changed anything")
}

// TestRaiseAllBucketReplicas_RefusesWhenClusterSizeIsUndeclared keeps the sweep
// from reading an undeclared size as "one replica is what everything wants",
// which would report a whole cluster of single-replica buckets as healthy.
func TestRaiseAllBucketReplicas_RefusesWhenClusterSizeIsUndeclared(t *testing.T) {
	js := newJetStream(t)
	clustersize.RedeclareForTest(t, 0)

	_, err := kvutil.RaiseAllBucketReplicas(t.Context(), js)
	require.ErrorIs(t, err, clustersize.ErrUndeclared)
}
