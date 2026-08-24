package handlers_ecs

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"

	"github.com/stretchr/testify/require"
)

func TestSchedulerLeaderRefresh(t *testing.T) {
	t.Run("stale revision is refused", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		kv, err := InitLeaderBucket(t.Context(), js)
		require.NoError(t, err)

		staleRev, err := kv.Create(t.Context(), schedulerLeaderKey, []byte("node-a"))
		require.NoError(t, err)

		//Bypass TTL
		require.NoError(t, kv.Delete(t.Context(), schedulerLeaderKey))
		_, err = kv.Create(t.Context(), schedulerLeaderKey, []byte("node-b"))
		require.NoError(t, err)

		require.False(t, refreshLease(t.Context(), kv, schedulerLeaderKey, "node-a", staleRev),
			"node-a refreshed a lease that node-b now holds")

		entry, err := kv.Get(t.Context(), schedulerLeaderKey)
		require.NoError(t, err)
		require.Equal(t, "node-b", string(entry.Value()))
	})

	t.Run("current revision succeeds", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		kv, err := InitLeaderBucket(t.Context(), js)
		require.NoError(t, err)

		rev, err := kv.Create(t.Context(), schedulerLeaderKey, []byte("node-a"))
		require.NoError(t, err)

		require.True(t, refreshLease(t.Context(), kv, schedulerLeaderKey, "node-a", rev))
	})

	t.Run("release at a stale revision is refused", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		kv, err := InitLeaderBucket(t.Context(), js)
		require.NoError(t, err)

		staleRev, err := kv.Create(t.Context(), schedulerLeaderKey, []byte("node-a"))
		require.NoError(t, err)

		//Bypass TTL
		require.NoError(t, kv.Delete(t.Context(), schedulerLeaderKey))
		_, err = kv.Create(t.Context(), schedulerLeaderKey, []byte("node-b"))
		require.NoError(t, err)

		require.Error(t, dropLease(t.Context(), kv, schedulerLeaderKey, staleRev),
			"node-a released a lease that node-b now holds")

		entry, err := kv.Get(t.Context(), schedulerLeaderKey)
		require.NoError(t, err)
		require.Equal(t, "node-b", string(entry.Value()))
	})

	t.Run("release at the current revision succeeds", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		kv, err := InitLeaderBucket(t.Context(), js)
		require.NoError(t, err)

		rev, err := kv.Create(t.Context(), schedulerLeaderKey, []byte("node-a"))
		require.NoError(t, err)

		require.NoError(t, dropLease(t.Context(), kv, schedulerLeaderKey, rev))

		_, err = kv.Get(t.Context(), schedulerLeaderKey)
		require.Error(t, err)
	})
}
