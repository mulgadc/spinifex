package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	instancecache "github.com/mulgadc/spinifex/spinifex/runtime/compute/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// levelManager opens both buckets on a server of its own, so the cluster-config
// record is not shared with parallel tests.
func levelManager(t *testing.T) (*JetStreamManager, *featurelevel.Store) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, m.InitClusterConfigBucket())
	require.NoError(t, m.InitClusterStateBucket())
	store, err := m.FeatureLevelStore(t.Context())
	require.NoError(t, err)
	return m, store
}

func levelBeat(t *testing.T, m *JetStreamManager, node string, level int, now time.Time, age time.Duration) {
	t.Helper()
	require.NoError(t, m.WriteHeartbeat(&Heartbeat{
		Node: node, Timestamp: now.Add(-age).UTC().Format(time.RFC3339), FeatureLevel: level,
	}))
}

func tick(t *testing.T, m *JetStreamManager, store *featurelevel.Store, nodes []string, now time.Time) (featurelevel.Record, bool) {
	t.Helper()
	rec, advanced, err := advanceFeatureLevel(t.Context(), store, m.ReadHeartbeat, nodes, now)
	require.NoError(t, err)
	return rec, advanced
}

func TestHeartbeatWithoutFeatureLevelReadsAsZeroAndBlocksAdvance(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	now := time.Now()
	// The shape an older build writes: no feature_level key at all.
	old := `{"node":"old","epoch":1,"timestamp":"` + now.UTC().Format(time.RFC3339) + `","services":["daemon"],"vm_count":0}`
	_, err := m.clusterKV.Put(t.Context(), clusterv1.HeartbeatKey("old"), []byte(old))
	require.NoError(t, err)
	h, err := m.ReadHeartbeat("old")
	require.NoError(t, err)
	assert.Zero(t, h.FeatureLevel)

	levelBeat(t, m, "new", featurelevel.Build, now, 0)
	rec, advanced := tick(t, m, store, []string{"new", "old"}, now)
	assert.False(t, advanced, "a member on an older build holds the cluster at 0")
	assert.Zero(t, rec.Level)
}

func TestFeatureLevelAdvancesOnceEveryMemberIsCurrent(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	now := time.Now()
	nodes := []string{"a", "b", "c"}
	levelBeat(t, m, "a", featurelevel.Build, now, 0)
	levelBeat(t, m, "b", featurelevel.Build, now, 0)

	rec, advanced := tick(t, m, store, nodes, now)
	assert.False(t, advanced, "member c has no heartbeat key (never written or expired from the 1h bucket)")
	assert.Zero(t, rec.Level)

	levelBeat(t, m, "c", featurelevel.Build, now, 0)
	rec, advanced = tick(t, m, store, nodes, now)
	assert.True(t, advanced)
	assert.Equal(t, featurelevel.Build, rec.Level)
}

func TestFeatureLevelStaleMemberBlocksAndLaterStalenessNeverLowers(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	now := time.Now()
	nodes := []string{"a", "b"}
	levelBeat(t, m, "a", featurelevel.Build, now, 0)
	levelBeat(t, m, "b", featurelevel.Build, now, instancecache.NodeStaleAfter+5*time.Second)

	rec, advanced := tick(t, m, store, nodes, now)
	assert.False(t, advanced, "a heartbeat older than NodeStaleAfter is not current")
	assert.Zero(t, rec.Level)

	levelBeat(t, m, "b", featurelevel.Build, now, instancecache.NodeStaleAfter-5*time.Second)
	rec, advanced = tick(t, m, store, nodes, now)
	assert.True(t, advanced)
	assert.Equal(t, featurelevel.Build, rec.Level)

	later := now.Add(10 * time.Minute)
	rec, advanced = tick(t, m, store, nodes, later)
	assert.False(t, advanced)
	assert.Equal(t, featurelevel.Build, rec.Level, "every member going stale afterwards does not lower the level")
}

func TestFeatureLevelReadErrorAdvancesNothing(t *testing.T) {
	t.Parallel()
	_, store := levelManager(t)
	boom := errors.New("kv unavailable")
	read := func(string) (*Heartbeat, error) { return nil, boom }
	rec, advanced, err := advanceFeatureLevel(t.Context(), store, read, []string{"a"}, time.Now())
	require.ErrorIs(t, err, boom)
	assert.False(t, advanced)
	assert.Zero(t, rec.Level)
	got, _, rerr := store.Read(t.Context())
	require.NoError(t, rerr)
	assert.Zero(t, got.Level)
}

func TestFeatureLevelUnparseableTimestampIsNotCurrent(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	require.NoError(t, m.WriteHeartbeat(&Heartbeat{Node: "a", Timestamp: "yesterday", FeatureLevel: featurelevel.Build}))
	_, advanced := tick(t, m, store, []string{"a"}, time.Now())
	assert.False(t, advanced)
}

func TestFeatureLevelStoreNeedsTheClusterConfigBucket(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	_, err = m.FeatureLevelStore(context.Background())
	require.Error(t, err)
}
