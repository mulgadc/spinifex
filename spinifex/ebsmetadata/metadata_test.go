package ebsmetadata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeMetadataRoundTripOwnsItsSchema(t *testing.T) {
	want := Volume{
		VolumeID: "vol-1", TenantID: "acct-1", CapacityGiB: 20,
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
		VolumeID: "vol-1", TenantID: "acct-1", CapacityGiB: 20,
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
		_, err := VolumeKey(id)
		require.Error(t, err, "ID %q must not escape metadata prefix", id)
	}
	key, err := AMIKey("ami-1")
	require.NoError(t, err)
	assert.Equal(t, "spinifex/ebsmetadata/v1/amis/ami-1.json", key)
}
