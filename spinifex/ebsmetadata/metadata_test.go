package ebsmetadata

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeMetadataRoundTripOwnsItsSchema(t *testing.T) {
	want := Volume{
		VolumeID: "vol-1", TenantID: "000000000001", CapacityGiB: 20,
		State: "available", CreatedAt: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
		ProviderHandle: "opaque-provider-handle", Tags: map[string]string{"env": "test"},
	}
	data, err := MarshalVolume(want)
	require.NoError(t, err)
	got, err := UnmarshalVolume(data)
	require.NoError(t, err)
	assert.Equal(t, SchemaVersion, got.SchemaVersion)
	assert.Equal(t, want.VolumeID, got.VolumeID)
	assert.Equal(t, want.ProviderHandle, got.ProviderHandle)
	assert.Equal(t, want.Tags, got.Tags)
}

// TestVolumeRoundTripsEncryptedAndModification covers the two fields added
// to close the schema gaps behind the provider-branch consumer bugs: a
// volume's encryption bit and its persisted modification record must both
// survive a marshal/unmarshal round-trip.
func TestVolumeRoundTripsEncryptedAndModification(t *testing.T) {
	want := Volume{
		VolumeID: "vol-1", TenantID: "000000000001", CapacityGiB: 20,
		State: "available", Encrypted: true,
		Modification: &VolumeModification{
			ModificationState: "completed", Progress: 100,
			OriginalSize: 8, OriginalIOPS: 3000, OriginalVolumeType: "gp3",
			TargetSize: 16, TargetIOPS: 3000, TargetVolumeType: "gp3",
			StartTime: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 8, 5, 0, 0, 1, 0, time.UTC),
		},
	}
	data, err := MarshalVolume(want)
	require.NoError(t, err)
	got, err := UnmarshalVolume(data)
	require.NoError(t, err)
	assert.True(t, got.Encrypted)
	require.NotNil(t, got.Modification)
	assert.Equal(t, *want.Modification, *got.Modification)
}

func TestMetadataRejectsUnknownSchemaVersion(t *testing.T) {
	_, err := UnmarshalAMI([]byte(`{"schema_version":99,"image_id":"ami-1"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestMetadataKeysRejectPathTraversal(t *testing.T) {
	for _, id := range []string{"", "..", "../escape", "a/b", "a\\b"} {
		_, err := VolumeKey("000000000001", id)
		require.Error(t, err, "ID %q must not escape metadata prefix", id)

		_, err = SnapshotKey("000000000001", id)
		require.Error(t, err, "ID %q must not escape metadata prefix", id)

		_, err = VolumeKey(id, "vol-1")
		require.Error(t, err, "account %q must not escape metadata prefix", id)
	}
	key, err := AMIKey("ami-1")
	require.NoError(t, err)
	assert.Equal(t, "spinifex/ebsmetadata/v2/amis/ami-1.json", key)
}

// An untenanted document cannot be written, so it cannot exist, so nothing
// needs to read one.
func TestPartitionedKeysRejectNonAccountOwner(t *testing.T) {
	for _, accountID := range []string{"", "acct-1", "12345678901", "1234567890123", "00000000000a", "system"} {
		_, err := VolumeKey(accountID, "vol-1")
		require.Error(t, err, "account %q is not an account ID", accountID)

		_, err = SnapshotKey(accountID, "snap-1")
		require.Error(t, err, "account %q is not an account ID", accountID)
	}

	// A transposed (id, account) pair: a vol- string is not twelve digits.
	_, err := VolumeKey("vol-1", "000000000001")
	require.Error(t, err)

	// The system account is an ordinary account and partitions like any other.
	key, err := VolumeKey(utils.GlobalAccountID, "vol-1")
	require.NoError(t, err)
	assert.Equal(t, "spinifex/ebsmetadata/v2/volumes/000000000000/vol-1.json", key)
}

func TestSnapshotKeyPartitionsByOwningAccount(t *testing.T) {
	key, err := SnapshotKey("000000000001", "snap-1")
	require.NoError(t, err)
	assert.Equal(t, "spinifex/ebsmetadata/v2/snapshots/000000000001/snap-1.json", key)

	other, err := SnapshotKey("000000000002", "snap-1")
	require.NoError(t, err)
	assert.NotEqual(t, key, other, "the same snapshot ID under two accounts must be two keys")
}

// The owning account is a key segment, which is what lets a listing of one
// account's prefix answer without reading another account's documents.
func TestVolumeKeyPartitionsByOwningAccount(t *testing.T) {
	key, err := VolumeKey("000000000001", "vol-1")
	require.NoError(t, err)
	assert.Equal(t, "spinifex/ebsmetadata/v2/volumes/000000000001/vol-1.json", key)

	other, err := VolumeKey("000000000002", "vol-1")
	require.NoError(t, err)
	assert.NotEqual(t, key, other, "the same volume ID under two accounts must be two keys")
}
