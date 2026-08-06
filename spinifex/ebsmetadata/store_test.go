package ebsmetadata

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreRoundTripsVolumeAndAMI(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")

	volume := Volume{VolumeID: "vol-1", TenantID: "acct-1", CapacityGiB: 10, State: "available", ProviderHandle: "opaque"}
	require.NoError(t, store.PutVolume(context.Background(), volume))
	gotVolume, err := store.GetVolume(context.Background(), volume.VolumeID)
	require.NoError(t, err)
	assert.Equal(t, volume.VolumeID, gotVolume.VolumeID)
	assert.Equal(t, volume.ProviderHandle, gotVolume.ProviderHandle)
	assert.Equal(t, SchemaVersion, gotVolume.SchemaVersion)

	ami := AMI{ImageID: "ami-1", Name: "test", SnapshotID: "snap-1"}
	require.NoError(t, store.PutAMI(context.Background(), ami))
	gotAMI, err := store.GetAMI(context.Background(), ami.ImageID)
	require.NoError(t, err)
	assert.Equal(t, ami.ImageID, gotAMI.ImageID)
	assert.Equal(t, ami.SnapshotID, gotAMI.SnapshotID)
	assert.Equal(t, SchemaVersion, gotAMI.SchemaVersion)

	require.NoError(t, store.DeleteVolume(context.Background(), volume.VolumeID))
	require.NoError(t, store.DeleteAMI(context.Background(), ami.ImageID))
}
