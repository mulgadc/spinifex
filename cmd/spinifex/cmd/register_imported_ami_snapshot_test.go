package cmd

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provider-backed import path (admin.ImportImage) only writes the
// storage backend's blocks, not the EC2 control plane's document, so what it
// writes must be readable by DescribeSnapshots and GetAMISourceVolumeID the
// same way CreateImageFromInstance's and CopyImage's snapshots are.
func TestRegisterImportedAMISnapshot_WritesEC2ReadableMetadata(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	const bucket = "predastore"
	ami := ebsmetadata.AMI{
		ImageID:         "ami-import01",
		SnapshotID:      "snap-ami-import01",
		Name:            "debian-13-x86_64",
		VolumeSizeGiB:   8,
		ImageOwnerAlias: "system",
	}

	require.NoError(t, registerImportedAMISnapshot(store, bucket, ami, "ap-southeast-2a", true))

	cfg, err := ebsmetadata.NewStore(store, bucket).GetSnapshot(context.Background(), utils.GlobalAccountID, "snap-ami-import01")
	require.NoError(t, err)
	assert.Equal(t, "snap-ami-import01", cfg.SnapshotID)
	assert.Equal(t, "ami-import01", cfg.VolumeID, "the import path creates the volume under the AMI's own ID")
	assert.Equal(t, int64(8), cfg.VolumeSize)
	assert.Equal(t, "completed", cfg.State)
	assert.Equal(t, "100%", cfg.Progress)
	assert.Equal(t, "ap-southeast-2a", cfg.AvailabilityZone)
	assert.True(t, cfg.Encrypted)
	assert.Equal(t, utils.GlobalAccountID, cfg.OwnerID, "a system-catalog import has no tenant account to own its snapshot")
	assert.Contains(t, cfg.Description, ami.Name)
}

// An unencrypted volume must not be advertised as encrypted, which would let
// a consumer skip a key it actually needs.
func TestRegisterImportedAMISnapshot_UnencryptedVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	ami := ebsmetadata.AMI{ImageID: "ami-plain01", SnapshotID: "snap-ami-plain01", VolumeSizeGiB: 4}

	require.NoError(t, registerImportedAMISnapshot(store, "predastore", ami, "ap-southeast-2a", false))

	cfg, err := ebsmetadata.NewStore(store, "predastore").GetSnapshot(context.Background(), utils.GlobalAccountID, "snap-ami-plain01")
	require.NoError(t, err)
	assert.False(t, cfg.Encrypted)
}

// A failed metadata write must surface: silently continuing would leave the
// AMI document referencing a snapshot ID DescribeSnapshots can never resolve.
func TestRegisterImportedAMISnapshot_WriteFailureSurfaces(t *testing.T) {
	store := &failingObjectStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), failPutObject: true}
	ami := ebsmetadata.AMI{ImageID: "ami-fail01", SnapshotID: "snap-ami-fail01", VolumeSizeGiB: 4}

	err := registerImportedAMISnapshot(store, "predastore", ami, "ap-southeast-2a", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "register snapshot metadata")
}
