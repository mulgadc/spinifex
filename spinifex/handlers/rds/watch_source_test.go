package handlers_rds

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAccountWatchBuckets_ReturnsOnePerAccountBucket also pins the exclusion: a
// bucket that is not an RDS account bucket must not be watched, or every write
// anywhere in the cluster would wake the DNS reconcile.
func TestAccountWatchBuckets_ReturnsOnePerAccountBucket(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	_, err := GetOrCreateAccountBucket(t.Context(), js, "111111111111")
	require.NoError(t, err)
	_, err = GetOrCreateAccountBucket(t.Context(), js, "222222222222")
	require.NoError(t, err)
	_, err = kvutil.GetOrCreateBucket(t.Context(), js, "something-else", 1)
	require.NoError(t, err)

	buckets, err := AccountWatchBuckets(t.Context(), js)
	require.NoError(t, err)

	names := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		names = append(names, bucket.Name())
	}
	assert.ElementsMatch(t, []string{
		AccountBucketName("111111111111"),
		AccountBucketName("222222222222"),
	}, names)
}

func TestAccountWatchBuckets_NoAccountsIsNotAnError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	buckets, err := AccountWatchBuckets(t.Context(), js)
	require.NoError(t, err, "a cluster with no RDS usage is not a failure")
	assert.Empty(t, buckets)
}

// TestAccountWatchBuckets_BucketsAreWatchable closes the loop: the handles are
// not merely named, they open against the live server.
func TestAccountWatchBuckets_BucketsAreWatchable(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	_, err := GetOrCreateAccountBucket(t.Context(), js, "111111111111")
	require.NoError(t, err)

	buckets, err := AccountWatchBuckets(t.Context(), js)
	require.NoError(t, err)
	require.Len(t, buckets, 1)

	watcher, err := buckets[0].Watch(t.Context(), ">")
	require.NoError(t, err)
	require.NoError(t, watcher.Stop())
}
