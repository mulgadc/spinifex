// Drives the current cluster-config open, migration, EBS lock and feature-level
// paths beside a readiness binding anchor, through the manager's unexported
// JetStream handle.
//
//test:in-package
package daemon

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadinessAnchorSurvivesClusterConfigReaders(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, m.InitClusterConfigBucket())
	_, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "bedrock-endpoints", History: 1})
	require.NoError(t, err)
	in := readiness.NewInitializer(js, readiness.InitConfig{ConfigBucket: ClusterConfigBucket, EndpointBucket: "bedrock-endpoints"})
	_, err = in.Initialize(t.Context(), readiness.FirstRollout{Confirmed: true})
	require.NoError(t, err)

	kv, err := js.KeyValue(t.Context(), ClusterConfigBucket)
	require.NoError(t, err)
	before, err := kv.Get(t.Context(), readiness.AnchorKey)
	require.NoError(t, err)
	st, err := js.Stream(t.Context(), "KV_"+ClusterConfigBucket)
	require.NoError(t, err)
	created := st.CachedInfo().Created

	// A second daemon opens and migrates the bucket and takes the EBS lock.
	other, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, other.InitClusterConfigBucket())
	require.NoError(t, migrate.DefaultRegistry.RunKV(t.Context(), ClusterConfigBucket, kv, ClusterConfigBucketVersion))
	_, err = m.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderQEMUNBD)
	require.NoError(t, err)
	_, err = other.ClaimEBSProvider(t.Context(), "node2", config.EBSProviderViperblockd)
	var mismatch *EBSProviderMismatchError
	require.ErrorAs(t, err, &mismatch)

	// The feature level advances, then a rollback is cancelled, restarted and finished.
	store, err := m.FeatureLevelStore(t.Context())
	require.NoError(t, err)
	members := []featurelevel.Member{{Node: "node1", Level: featurelevel.Build, Current: true}}
	_, advanced, err := store.Advance(t.Context(), members)
	require.NoError(t, err)
	require.True(t, advanced)
	_, _, err = store.StartRollback(t.Context(), featurelevel.Build-1, featurelevel.Build)
	require.NoError(t, err)
	_, err = store.CancelRollback(t.Context())
	require.NoError(t, err)
	_, rev, err := store.StartRollback(t.Context(), featurelevel.Build-1, featurelevel.Build)
	require.NoError(t, err)
	_, err = store.FinishRollback(t.Context(), rev, featurelevel.Build-1)
	require.NoError(t, err)
	_, err = store.ReleaseHold(t.Context())
	require.NoError(t, err)
	attached, err := m.AttachFeatureLevel(t.Context())
	require.NoError(t, err)
	_, _, err = attached.Read(t.Context())
	require.NoError(t, err)
	kvstore.TryRaiseBucketReplicas(t.Context(), js, ClusterConfigBucket, 1)

	after, err := kv.Get(t.Context(), readiness.AnchorKey)
	require.NoError(t, err)
	assert.Equal(t, before.Revision(), after.Revision())
	assert.Equal(t, before.Value(), after.Value())
	version, err := kvstore.ReadVersion(t.Context(), kv)
	require.NoError(t, err)
	assert.Equal(t, ClusterConfigBucketVersion, version, "the anchor needs no schema bump")
	st, err = js.Stream(t.Context(), "KV_"+ClusterConfigBucket)
	require.NoError(t, err)
	assert.True(t, created.Equal(st.CachedInfo().Created))
	_, err = in.Load(t.Context())
	require.NoError(t, err)
}

func TestClusterConfigAheadOfThisBuildIsRefused(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, m.InitClusterConfigBucket())
	kv, err := js.KeyValue(t.Context(), ClusterConfigBucket)
	require.NoError(t, err)
	require.NoError(t, kvstore.WriteVersion(t.Context(), kv, ClusterConfigBucketVersion+1))

	err = migrate.DefaultRegistry.RunKV(t.Context(), ClusterConfigBucket, kv, ClusterConfigBucketVersion)
	var ahead migrate.SchemaAheadError
	require.ErrorAs(t, err, &ahead, "a stamped version above this build stops every older open, so the anchor must not bump it")
}
