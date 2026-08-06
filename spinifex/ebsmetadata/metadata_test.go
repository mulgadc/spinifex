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
