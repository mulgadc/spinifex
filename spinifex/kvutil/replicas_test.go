package kvutil

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditBucketReplicas_ReportsEveryBucketAgainstTheClusterSize(t *testing.T) {
	js := startJetStream(t)

	for _, bucket := range []string{"audit-alpha", "audit-beta"} {
		_, err := GetOrCreateBucket(t.Context(), js, bucket, 1)
		require.NoError(t, err)
	}

	reports, err := AuditBucketReplicas(t.Context(), js)
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
	clustersize.Declare(3)
	t.Cleanup(func() { clustersize.Declare(1) })

	reports, err = AuditBucketReplicas(t.Context(), js)
	require.NoError(t, err)
	require.Len(t, reports, 2)
	for _, r := range reports {
		assert.Equal(t, 1, r.Replicas)
		assert.Equal(t, 3, r.Want)
		assert.True(t, r.UnderReplicated(), "%s", r.Bucket)
	}
}

func TestAuditBucketReplicas_RefusesWhenClusterSizeIsUndeclared(t *testing.T) {
	js := startJetStream(t)
	clustersize.Declare(0)
	t.Cleanup(func() { clustersize.Declare(1) })

	_, err := AuditBucketReplicas(t.Context(), js)
	require.ErrorIs(t, err, clustersize.ErrUndeclared)
}

// TestRaiseBucketReplicas_NeverLowers is the property that makes the raise safe
// to run on every service start: a bucket already at or above the count is left
// alone, so a cluster that has lost a node does not have its durability cut to
// match.
func TestRaiseBucketReplicas_NeverLowers(t *testing.T) {
	js := startJetStream(t)

	_, err := GetOrCreateBucket(t.Context(), js, "no-downgrade", 1)
	require.NoError(t, err)

	require.NoError(t, RaiseBucketReplicas(t.Context(), js, "no-downgrade", 1))
	assert.Equal(t, 1, streamReplicas(t, js, "no-downgrade"))

	// Zero and negative are below the current count, so they are no-ops too
	// rather than an update that would ask the server for an invalid config.
	require.NoError(t, RaiseBucketReplicas(t.Context(), js, "no-downgrade", 0))
	assert.Equal(t, 1, streamReplicas(t, js, "no-downgrade"))
}

// TestRaiseBucketReplicas_PreservesTheRestOfTheConfig is why this updates the
// stream rather than the KV bucket: a KeyValueConfig cannot express a stream's
// full configuration, so applying one would reset whatever it omits.
func TestRaiseBucketReplicas_PreservesTheRestOfTheConfig(t *testing.T) {
	js := startJetStream(t)

	_, err := GetOrCreateBucketWithOptions(t.Context(), js, BucketOptions{
		Name:        "config-preserved",
		Description: "a bucket with settings worth not losing",
		History:     7,
	})
	require.NoError(t, err)

	before := streamConfig(t, js, "config-preserved")
	require.NoError(t, RaiseBucketReplicas(t.Context(), js, "config-preserved", 1))
	after := streamConfig(t, js, "config-preserved")

	assert.Equal(t, before.Description, after.Description)
	assert.Equal(t, before.MaxMsgsPerSubject, after.MaxMsgsPerSubject, "history is MaxMsgsPerSubject on the stream")
	assert.Equal(t, before.MaxAge, after.MaxAge)
}

// TestRaiseBucketReplicas_SurfacesAMissingBucket keeps a raise against a bucket
// that is not there from reading as success.
func TestRaiseBucketReplicas_SurfacesAMissingBucket(t *testing.T) {
	js := startJetStream(t)

	err := RaiseBucketReplicas(t.Context(), js, "never-created", 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, jetstream.ErrStreamNotFound)
}

func streamConfig(t *testing.T, js jetstream.JetStream, bucket string) jetstream.StreamConfig {
	t.Helper()
	stream, err := js.Stream(t.Context(), streamPrefix+bucket)
	require.NoError(t, err)
	info, err := stream.Info(t.Context())
	require.NoError(t, err)
	return info.Config
}
