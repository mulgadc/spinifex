package handlers_eks

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleAddonRecord(name string) *AddonRecord {
	now := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	return &AddonRecord{
		AddonName:    name,
		AddonVersion: "1.0.0",
		Status:       AddonStatusCreating,
		Arn:          "arn:aws:eks:us-east-1:111122223333:addon/c1/" + name,
		CreatedAt:    now,
		ModifiedAt:   now,
	}
}

func TestAddonRecordGuards(t *testing.T) {
	require.Error(t, PutAddonRecord(t.Context(), nil, "c1", nil))
	require.Error(t, PutAddonRecord(t.Context(), nil, "", sampleAddonRecord("x")))
	require.Error(t, PutAddonRecord(t.Context(), nil, "c1", &AddonRecord{}))

	_, err := GetAddonRecord(t.Context(), nil, "", "x")
	require.Error(t, err)

	_, err = ListAddonRecords(t.Context(), nil, "")
	require.Error(t, err)
}

func TestAddonRecord_RoundTrip(t *testing.T) {
	kv := newClusterStateTestKV(t)

	in := sampleAddonRecord("aws-load-balancer-controller")
	in.Tags = map[string]string{"team": "platform"}
	require.NoError(t, PutAddonRecord(t.Context(), kv, "c1", in))

	got, err := GetAddonRecord(t.Context(), kv, "c1", in.AddonName)
	require.NoError(t, err)
	assert.Equal(t, in.AddonName, got.AddonName)
	assert.Equal(t, in.AddonVersion, got.AddonVersion)
	assert.Equal(t, in.Status, got.Status)
	assert.Equal(t, "platform", got.Tags["team"])
}

func TestGetAddonRecord_NotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)
	_, err := GetAddonRecord(t.Context(), kv, "c1", "ghost")
	assert.ErrorIs(t, err, ErrAddonNotFound)
}

func TestListAddonRecords_SkipsManifestSubKeys(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.NoError(t, PutAddonRecord(t.Context(), kv, "c1", sampleAddonRecord("coredns")))
	require.NoError(t, PutAddonRecord(t.Context(), kv, "c1", sampleAddonRecord("aws-load-balancer-controller")))
	// A staged manifest sub-key must not be mistaken for a record.
	_, err := kv.Put(t.Context(), AddonManifestKey("c1", "coredns"), []byte(`{"addonName":"coredns"}`))
	require.NoError(t, err)

	recs, err := ListAddonRecords(t.Context(), kv, "c1")
	require.NoError(t, err)
	require.Len(t, recs, 2)
	// Sorted by name.
	assert.Equal(t, "aws-load-balancer-controller", recs[0].AddonName)
	assert.Equal(t, "coredns", recs[1].AddonName)
}

func TestDeleteAddonRecord_RemovesRecordAndManifest(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutAddonRecord(t.Context(), kv, "c1", sampleAddonRecord("coredns")))
	_, err := kv.Put(t.Context(), AddonManifestKey("c1", "coredns"), []byte(`{}`))
	require.NoError(t, err)

	require.NoError(t, DeleteAddonRecord(t.Context(), kv, "c1", "coredns"))

	_, err = GetAddonRecord(t.Context(), kv, "c1", "coredns")
	assert.ErrorIs(t, err, ErrAddonNotFound)
	_, err = kv.Get(t.Context(), AddonManifestKey("c1", "coredns"))
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	// Deleting again is a not-found.
	assert.ErrorIs(t, DeleteAddonRecord(t.Context(), kv, "c1", "coredns"), ErrAddonNotFound)
}

func TestCasUpdateAddon(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutAddonRecord(t.Context(), kv, "c1", sampleAddonRecord("coredns")))

	updated, err := casUpdateAddon(t.Context(), kv, "c1", "coredns", func(r *AddonRecord) bool {
		r.Status = AddonStatusActive
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, AddonStatusActive, updated.Status)

	got, err := GetAddonRecord(t.Context(), kv, "c1", "coredns")
	require.NoError(t, err)
	assert.Equal(t, AddonStatusActive, got.Status)

	_, err = casUpdateAddon(t.Context(), kv, "c1", "ghost", func(*AddonRecord) bool { return true })
	assert.ErrorIs(t, err, ErrAddonNotFound)
}
