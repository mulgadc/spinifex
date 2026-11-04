package addon

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "111122223333"

func newTestKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "addon-owner-test"})
	require.NoError(t, err)
	return kv
}

func sampleRecord(name string) *Record {
	now := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	return &Record{
		AddonName:    name,
		AddonVersion: "1.0.0",
		Status:       StatusCreating,
		Arn:          "arn:aws:eks:us-east-1:111122223333:addon/c1/" + name,
		CreatedAt:    now,
		ModifiedAt:   now,
	}
}

func TestKeyPaths(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "clusters/c1/addons/", Prefix("c1"))
	assert.Equal(t, "clusters/c1/addons/argocd", Key("c1", "argocd"))
	assert.Equal(t, "clusters/c1/addons/argocd/manifest", ManifestKey("c1", "argocd"))
}

// The guard clauses reject malformed input before touching the KV, so a nil
// handle is enough to exercise them.
func TestRecordGuards(t *testing.T) {
	t.Parallel()
	require.Error(t, put(t.Context(), nil, "c1", nil))
	require.Error(t, put(t.Context(), nil, "", sampleRecord("x")))
	require.Error(t, put(t.Context(), nil, "c1", &Record{}))

	_, err := Get(t.Context(), nil, "", "x")
	require.Error(t, err)

	_, err = List(t.Context(), nil, "")
	require.Error(t, err)
}

func TestRecord_RoundTrip(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)

	in := sampleRecord("aws-load-balancer-controller")
	in.Tags = map[string]string{"team": "platform"}
	require.NoError(t, put(t.Context(), kv, "c1", in))

	got, err := Get(t.Context(), kv, "c1", in.AddonName)
	require.NoError(t, err)
	assert.Equal(t, in.AddonName, got.AddonName)
	assert.Equal(t, in.AddonVersion, got.AddonVersion)
	assert.Equal(t, in.Status, got.Status)
	assert.Equal(t, "platform", got.Tags["team"])
}

func TestGet_NotFound(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	_, err := Get(t.Context(), kv, "c1", "ghost")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestList_SkipsManifestSubKeys(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)

	require.NoError(t, put(t.Context(), kv, "c1", sampleRecord("coredns")))
	require.NoError(t, put(t.Context(), kv, "c1", sampleRecord("aws-load-balancer-controller")))
	// A staged manifest sub-key must not be mistaken for a record.
	_, err := kv.Put(t.Context(), ManifestKey("c1", "coredns"), []byte(`{"addonName":"coredns"}`))
	require.NoError(t, err)

	recs, err := List(t.Context(), kv, "c1")
	require.NoError(t, err)
	require.Len(t, recs, 2)
	// Sorted by name.
	assert.Equal(t, "aws-load-balancer-controller", recs[0].AddonName)
	assert.Equal(t, "coredns", recs[1].AddonName)
}

func TestListManifests_SortedAndScopedToCluster(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)

	got, err := ListManifests(t.Context(), kv, "c1")
	require.NoError(t, err)
	assert.NotNil(t, got, "an empty bucket lists as an empty slice, not nil")
	assert.Empty(t, got)

	for _, name := range []string{"coredns", "argocd"} {
		require.NoError(t, putManifest(t.Context(), kv, "c1", sampleRecord(name)))
	}
	require.NoError(t, putManifest(t.Context(), kv, "c2", sampleRecord("other")))
	require.NoError(t, put(t.Context(), kv, "c1", sampleRecord("argocd")))

	got, err = ListManifests(t.Context(), kv, "c1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "argocd", got[0].AddonName)
	assert.Equal(t, "coredns", got[1].AddonName)
}

func TestDeleteRecord_RemovesRecordAndManifest(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	require.NoError(t, put(t.Context(), kv, "c1", sampleRecord("coredns")))
	_, err := kv.Put(t.Context(), ManifestKey("c1", "coredns"), []byte(`{}`))
	require.NoError(t, err)

	require.NoError(t, deleteRecord(t.Context(), kv, "c1", "coredns"))

	_, err = Get(t.Context(), kv, "c1", "coredns")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = kv.Get(t.Context(), ManifestKey("c1", "coredns"))
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	// Deleting again is a not-found.
	assert.ErrorIs(t, deleteRecord(t.Context(), kv, "c1", "coredns"), ErrNotFound)
}

func TestCasUpdate(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	require.NoError(t, put(t.Context(), kv, "c1", sampleRecord("coredns")))

	updated, err := casUpdate(t.Context(), kv, "c1", "coredns", func(r *Record) bool {
		r.Status = StatusActive
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, StatusActive, updated.Status)

	got, err := Get(t.Context(), kv, "c1", "coredns")
	require.NoError(t, err)
	assert.Equal(t, StatusActive, got.Status)

	_, err = casUpdate(t.Context(), kv, "c1", "ghost", func(*Record) bool { return true })
	assert.ErrorIs(t, err, ErrNotFound)
}
