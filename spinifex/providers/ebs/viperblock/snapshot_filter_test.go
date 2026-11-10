package viperblock

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	vbtypes "github.com/mulgadc/viperblock/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repeat of CreateSnapshot must return the snapshot the first call took. A
// retake would fix it over writes made after the first answer, which a caller
// that released a quiesce on that answer relies on it not containing.
func TestCreateSnapshotWithVB_RepeatReturnsTheFirstSnapshot(t *testing.T) {
	const volumeName = "vol-snaprepeat0001"
	const snapshotID = "snap-repeat0000001"
	vb := createTestVBWithState(t, volumeName)

	first, err := createSnapshotWithVB(vb, volumeName, snapshotID)
	require.NoError(t, err)
	firstConfig, err := vb.Backend.ReadFrom(snapshotID, vbtypes.FileTypeConfig, 0, 0, 0)
	require.NoError(t, err)

	again, err := createSnapshotWithVB(vb, volumeName, snapshotID)
	require.NoError(t, err)
	againConfig, err := vb.Backend.ReadFrom(snapshotID, vbtypes.FileTypeConfig, 0, 0, 0)
	require.NoError(t, err)

	assert.Equal(t, firstConfig, againConfig, "the repeat rewrote the snapshot instead of returning it")
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, first.SourceVolumeID, again.SourceVolumeID)
	assert.Equal(t, first.Handle, again.Handle)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, again.State)
}

func TestProviderHandlers_ListSnapshots_FilteredOnOneSnapshot(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	putTestObject(t, store, cfg.Bucket, "snap-filterheld0001/config.json")
	putTestObject(t, store, cfg.Bucket, "snap-filterheld0001/checkpoints/blocks.00000001.bin")
	// A create that failed after its checkpoint and before its config.
	putTestObject(t, store, cfg.Bucket, "snap-filterpartial01/checkpoints/blocks.00000001.bin")
	putTestObject(t, store, cfg.Bucket, "snap-filterheld0001x/config.json")

	tests := []struct {
		name       string
		version    uint16
		snapshotID string
		want       []ebsprovider.SnapshotRef
		wantCode   ebsprovider.ErrorCode
	}{
		{name: "held snapshot is the one ref, completed", version: ebsprovider.SnapshotFilterSchemaVersion, snapshotID: "snap-filterheld0001",
			want: []ebsprovider.SnapshotRef{{ID: "snap-filterheld0001", Handle: snapshotHandle("snap-filterheld0001"), State: ebsprovider.SnapshotStateCompleted}}},
		{name: "snapshot without a config is not held", version: ebsprovider.SnapshotFilterSchemaVersion, snapshotID: "snap-filterpartial01"},
		{name: "absent snapshot answers none", version: ebsprovider.SnapshotFilterSchemaVersion, snapshotID: "snap-filterabsent01"},
		{name: "a volume ID names no snapshot", version: ebsprovider.SnapshotFilterSchemaVersion, snapshotID: "vol-filternotasnap1"},
		{name: "an unsafe ID names no snapshot", version: ebsprovider.SnapshotFilterSchemaVersion, snapshotID: "snap-../config"},
		{name: "filter below its version is unsupported", version: ebsprovider.NoSealSchemaVersion, snapshotID: "snap-filterheld0001",
			wantCode: ebsprovider.ErrorCodeUnsupportedVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := marshalRequest(t, map[string]any{"schema_version": tt.version, "snapshot_id": tt.snapshotID})
			msg := requestProvider(t, nc, ebsprovider.ListSnapshotsSubject, body)

			var resp ebsprovider.ListSnapshotsResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			if tt.wantCode != "" {
				require.NotNil(t, resp.Error)
				assert.Equal(t, tt.wantCode, resp.Error.Code)
				return
			}
			require.Nil(t, resp.Error)
			assert.Equal(t, tt.want, resp.Snapshots)
			assert.Empty(t, resp.NextToken)
		})
	}
}

// Enumeration reports a snapshot's state only to a request at the version that
// defines it, so an older caller's reply keeps the shape it was written for.
func TestProviderHandlers_ListSnapshots_StateFromItsVersion(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })
	putTestObject(t, store, cfg.Bucket, "snap-liststate00001/config.json")

	for version, want := range map[uint16]ebsprovider.SnapshotState{
		ebsprovider.NoSealSchemaVersion:         "",
		ebsprovider.SnapshotFilterSchemaVersion: ebsprovider.SnapshotStateCompleted,
	} {
		body := marshalRequest(t, map[string]any{"schema_version": version})
		msg := requestProvider(t, nc, ebsprovider.ListSnapshotsSubject, body)
		var resp ebsprovider.ListSnapshotsResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		require.Nil(t, resp.Error)
		require.Len(t, resp.Snapshots, 1)
		assert.Equalf(t, want, resp.Snapshots[0].State, "v%d", version)
	}
}
