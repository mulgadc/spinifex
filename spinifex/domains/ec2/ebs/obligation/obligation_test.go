package ebsobligation_test

import (
	"errors"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ebsobligation "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/obligation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *ebsobligation.Store {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store := ebsobligation.NewStore(js)
	require.NoError(t, store.Open(t.Context()))
	return store
}

func obligation(node, volumeID string, attachedAt time.Time) ebsobligation.UnpublishObligation {
	return ebsobligation.UnpublishObligation{
		VolumeID: volumeID, AccountID: "123456789012", NodeID: node,
		InstanceID: "i-1", AttachedAt: attachedAt, RecordedAt: attachedAt.Add(time.Minute),
	}
}

func TestKey_NamesNodeVolumeAndAttachment(t *testing.T) {
	at := time.Unix(1_700_000_000, 5)
	key, err := ebsobligation.Key(obligation("node-a", "vol-1", at))
	require.NoError(t, err)
	assert.Equal(t, "node-a.vol-1.1700000000000000005", key)

	key, err = ebsobligation.Key(obligation("node-a", "vol-1", time.Time{}))
	require.NoError(t, err)
	assert.Equal(t, "node-a.vol-1.0", key, "an unknown attachment time is 0, not a huge negative")
}

// A segment holding a subject separator or wildcard would split the key or
// let one node's listing match another's.
func TestKey_RejectsSegmentsThatAreNotOneToken(t *testing.T) {
	for _, o := range []ebsobligation.UnpublishObligation{
		obligation("node.a", "vol-1", time.Time{}),
		obligation("node-a", "vol.1", time.Time{}),
		obligation("*", "vol-1", time.Time{}),
		obligation("node-a", ">", time.Time{}),
		obligation("", "vol-1", time.Time{}),
	} {
		_, err := ebsobligation.Key(o)
		assert.Error(t, err, "%s/%s", o.NodeID, o.VolumeID)
	}
}

func TestStore_RecordIsIdempotentPerAttachment(t *testing.T) {
	store := newStore(t)
	at := time.Unix(1_700_000_000, 0).UTC()
	first := obligation("node-a", "vol-1", at)
	first.Handle = "h1"
	require.NoError(t, store.Record(t.Context(), first))
	retry := first
	retry.RecordedAt = at.Add(time.Hour)
	require.NoError(t, store.Record(t.Context(), retry), "a retry of the same obligation is not an error")
	require.NoError(t, store.Record(t.Context(), obligation("node-a", "vol-1", at.Add(time.Second))),
		"a different attachment of the same volume is a different obligation")

	open, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, open, 2)
	for _, o := range open {
		assert.Equal(t, ebsobligation.SchemaVersion, o.SchemaVersion)
		if o.AttachedAt.Equal(at) {
			assert.True(t, o.RecordedAt.Equal(at.Add(time.Minute)), "the first write is kept")
		}
	}
}

// The bucket's version stamp is a key in the same bucket; listing must not
// return it as an obligation.
func TestStore_ListSkipsTheVersionStampAndListNodeIsolatesNodes(t *testing.T) {
	store := newStore(t)
	at := time.Unix(1_700_000_000, 0).UTC()
	require.NoError(t, store.Record(t.Context(), obligation("node-a", "vol-1", at)))
	require.NoError(t, store.Record(t.Context(), obligation("node-a", "vol-2", at)))
	require.NoError(t, store.Record(t.Context(), obligation("node-ab", "vol-3", at)))

	all, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, all, 3)

	mine, err := store.ListNode(t.Context(), "node-a")
	require.NoError(t, err)
	volumes := []string{}
	for _, o := range mine {
		volumes = append(volumes, o.VolumeID)
	}
	assert.ElementsMatch(t, []string{"vol-1", "vol-2"}, volumes, "a node whose name prefixes another's sees only its own")

	_, err = store.ListNode(t.Context(), "*")
	assert.Error(t, err)
}

func TestStore_CloseAndRecordFailure(t *testing.T) {
	store := newStore(t)
	o := obligation("node-a", "vol-1", time.Unix(1_700_000_000, 0).UTC())
	require.NoError(t, store.Record(t.Context(), o))

	require.NoError(t, store.RecordFailure(t.Context(), o, errors.New("first")))
	require.NoError(t, store.RecordFailure(t.Context(), o, errors.New("second")))
	open, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, 2, open[0].Attempts)
	assert.Equal(t, "second", open[0].Error)

	require.NoError(t, store.Close(t.Context(), o))
	require.NoError(t, store.Close(t.Context(), o), "closing one already gone is not an error")
	require.NoError(t, store.RecordFailure(t.Context(), o, errors.New("late")),
		"a failure recorded after another sweep closed it does not resurrect it")
	open, err = store.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, open)
}

func TestListOpen_MissingBucketIsEmptyAndNotCreated(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	open, err := ebsobligation.ListOpen(t.Context(), nc)
	require.NoError(t, err)
	assert.Empty(t, open)
	_, err = js.KeyValue(t.Context(), ebsobligation.Bucket)
	assert.Error(t, err, "an operator listing must not create the bucket")

	store := ebsobligation.NewStore(js)
	require.NoError(t, store.Open(t.Context()))
	require.NoError(t, store.Record(t.Context(), obligation("node-a", "vol-1", time.Unix(1_700_000_000, 0).UTC())))
	open, err = ebsobligation.ListOpen(t.Context(), nc)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "vol-1", open[0].VolumeID)
}
