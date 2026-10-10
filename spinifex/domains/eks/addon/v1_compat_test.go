package addon

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newerRecordJSON is a record as a later binary might store it: every v1 field
// plus fields this binary's Record does not declare.
const newerRecordJSON = `{"addonName":"argocd","addonVersion":"3.0.23","status":"ACTIVE",` +
	`"arn":"arn:aws:eks:us-east-1:111122223333:addon/c1/argocd",` +
	`"createdAt":"2026-06-08T00:00:00Z","modifiedAt":"2026-06-08T00:00:00Z",` +
	`"incarnationId":"inc-1","generation":4,"observedGeneration":3,"origin":"user"}`

const newerManifestJSON = `{"addonName":"argocd","addonVersion":"3.0.23",` +
	`"incarnationId":"inc-1","generation":4}`

func seedNewerRecord(t *testing.T, kv jetstream.KeyValue) {
	t.Helper()
	_, err := kv.Put(t.Context(), "clusters/c1/addons/argocd", []byte(newerRecordJSON))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "clusters/c1/addons/argocd/manifest", []byte(newerManifestJSON))
	require.NoError(t, err)
}

func storedKeys(t *testing.T, kv jetstream.KeyValue, key string) []string {
	t.Helper()
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(entry.Value(), &top))
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Every owner write is decode-into-Record then re-encode, so fields this
// binary does not declare are erased from the record and the re-staged manifest.
func TestOwnerWrites_DropUnknownRecordAndManifestFields(t *testing.T) {
	t.Parallel()
	failed := "image pull"
	for _, tc := range []struct {
		name         string
		write        func(t *testing.T, o *Owner, kv jetstream.KeyValue)
		wantRecord   []string
		wantManifest []string
	}{
		{
			name: "Update",
			write: func(t *testing.T, o *Owner, kv jetstream.KeyValue) {
				_, err := o.Update(t.Context(), kv, testAccountID, "c1", "argocd", Change{})
				require.NoError(t, err)
			},
			wantRecord:   []string{"addonName", "addonVersion", "arn", "createdAt", "modifiedAt", "status"},
			wantManifest: []string{"addonName", "addonVersion"},
		},
		{
			name: "ApplyReport failed",
			write: func(t *testing.T, o *Owner, kv jetstream.KeyValue) {
				require.NoError(t, o.ApplyReport(t.Context(), kv, "c1", Report{Addon: "argocd", Phase: PhaseFailed, Message: failed}))
			},
			wantRecord:   []string{"addonName", "addonVersion", "arn", "createdAt", "health", "modifiedAt", "status"},
			wantManifest: []string{"addonName", "addonVersion", "generation", "incarnationId"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kv := newTestKV(t)
			seedNewerRecord(t, kv)
			o := New("us-east-1", NewStagingInstaller(func(_ context.Context, _ string) (jetstream.KeyValue, error) { return kv, nil }))
			tc.write(t, o, kv)
			assert.Equal(t, tc.wantRecord, storedKeys(t, kv, "clusters/c1/addons/argocd"))
			assert.Equal(t, tc.wantManifest, storedKeys(t, kv, "clusters/c1/addons/argocd/manifest"))
		})
	}
}

// ApplyReport skips the write when nothing changes, so a no-op report leaves
// unknown fields in place.
func TestOwnerApplyReport_NoOpKeepsUnknownFields(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	seedNewerRecord(t, kv)
	o := New("us-east-1", &fakeInstaller{})

	require.NoError(t, o.ApplyReport(t.Context(), kv, "c1", Report{Addon: "argocd", Phase: PhaseReady}))

	entry, err := kv.Get(t.Context(), "clusters/c1/addons/argocd")
	require.NoError(t, err)
	assert.JSONEq(t, newerRecordJSON, string(entry.Value()))
}

func TestOwnerDelete_ErasesRecordAndManifestWithUnknownFields(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	seedNewerRecord(t, kv)
	inst := &fakeInstaller{}
	o := New("us-east-1", inst)

	rec, err := o.Delete(t.Context(), kv, testAccountID, "c1", "argocd")
	require.NoError(t, err)
	assert.Equal(t, StatusDeleting, rec.Status)
	assert.Equal(t, []string{"argocd"}, inst.uninstalls)

	_, err = kv.Get(t.Context(), "clusters/c1/addons/argocd")
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	_, err = kv.Get(t.Context(), "clusters/c1/addons/argocd/manifest")
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

// Reads ignore unknown fields and never write back.
func TestReads_IgnoreUnknownFieldsWithoutRewriting(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	seedNewerRecord(t, kv)

	rec, err := Get(t.Context(), kv, "c1", "argocd")
	require.NoError(t, err)
	assert.Equal(t, StatusActive, rec.Status)

	recs, err := List(t.Context(), kv, "c1")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "3.0.23", recs[0].AddonVersion)

	ms, err := ListManifests(t.Context(), kv, "c1")
	require.NoError(t, err)
	assert.Equal(t, []Manifest{{AddonName: "argocd", AddonVersion: "3.0.23"}}, ms)

	entry, err := kv.Get(t.Context(), "clusters/c1/addons/argocd")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), entry.Revision())
}

// A field whose JSON type changes is not ignored: one undecodable record fails
// the whole cluster listing, and one undecodable manifest fails guest delivery.
func TestList_OneUndecodableEntryFailsTheListing(t *testing.T) {
	t.Parallel()
	kv := newTestKV(t)
	require.NoError(t, put(t.Context(), kv, "c1", sampleRecord("argocd")))
	_, err := kv.Put(t.Context(), "clusters/c1/addons/coredns", []byte(`{"addonName":"coredns","status":1}`))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "clusters/c1/addons/coredns/manifest", []byte(`{"addonName":"coredns","addonVersion":2}`))
	require.NoError(t, err)

	_, err = List(t.Context(), kv, "c1")
	require.ErrorContains(t, err, "unmarshal addon clusters/c1/addons/coredns")
	_, err = ListManifests(t.Context(), kv, "c1")
	require.ErrorContains(t, err, "unmarshal staged manifest clusters/c1/addons/coredns/manifest")
}
