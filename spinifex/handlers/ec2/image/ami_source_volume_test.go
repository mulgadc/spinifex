package handlers_ec2_image

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putSourceVolumeAMI registers an AMI document plus, when volumeID is set, the
// snapshot metadata recording which volume the snapshot was taken from.
func putSourceVolumeAMI(t *testing.T, svc *ImageServiceImpl, store *objectstore.MemoryObjectStore, ami ebsmetadata.AMI, volumeID string) {
	t.Helper()
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ami))
	if volumeID == "" {
		return
	}
	require.NoError(t, handlers_ec2_snapshot.WriteSnapshotConfig(store, testBucket, ami.SnapshotID,
		&handlers_ec2_snapshot.SnapshotConfig{SnapshotID: ami.SnapshotID, VolumeID: volumeID}))
}

// TestGetAMISourceVolumeID_ReadsSnapshotMetadata locks the normal case: the
// source volume comes from the snapshot's own metadata.json.
func TestGetAMISourceVolumeID_ReadsSnapshotMetadata(t *testing.T) {
	svc, store := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, store, ebsmetadata.AMI{
		ImageID: "ami-src01", SnapshotID: "snap-src01", ImageOwnerAlias: testAccountID,
	}, "vol-origin")

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-src01")
	require.NoError(t, err)
	assert.Equal(t, "vol-origin", got)
}

// TestGetAMISourceVolumeID_BundledSystemAMI locks the fallback for bundled
// system AMIs, whose snapshot is named after the AMI and carries no metadata.json.
func TestGetAMISourceVolumeID_BundledSystemAMI(t *testing.T) {
	svc, store := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, store, ebsmetadata.AMI{
		ImageID: "ami-sys01", SnapshotID: "snap-ami-sys01", ImageOwnerAlias: "system",
	}, "")

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-sys01")
	require.NoError(t, err)
	assert.Equal(t, "ami-sys01", got, "the bundled AMI's snapshot reads chunks from a volume named after the AMI")
}

// TestGetAMISourceVolumeID_AccountAMIMissingSnapshotMetadata locks that the
// bundled fallback does not apply to account-owned AMIs.
func TestGetAMISourceVolumeID_AccountAMIMissingSnapshotMetadata(t *testing.T) {
	svc, store := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, store, ebsmetadata.AMI{
		ImageID: "ami-acct01", SnapshotID: "snap-acct01", ImageOwnerAlias: testAccountID,
	}, "")

	_, err := svc.GetAMISourceVolumeID(context.Background(), "ami-acct01")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
}

// TestGetAMISourceVolumeID_AMIWithoutSnapshot locks that an AMI carrying no
// snapshot reference is reported as not found rather than resolving to "".
func TestGetAMISourceVolumeID_AMIWithoutSnapshot(t *testing.T) {
	svc, store := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, store, ebsmetadata.AMI{
		ImageID: "ami-nosnap", ImageOwnerAlias: testAccountID,
	}, "")

	_, err := svc.GetAMISourceVolumeID(context.Background(), "ami-nosnap")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

// TestGetAMISourceVolumeID_UnknownAMI locks the missing-AMI mapping.
func TestGetAMISourceVolumeID_UnknownAMI(t *testing.T) {
	svc, _ := setupProviderImageService(t)

	_, err := svc.GetAMISourceVolumeID(context.Background(), "ami-nothere")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

// TestGetAMISourceVolumeID_EmptySnapshotSourceVolume locks that corrupt
// snapshot metadata naming no source volume fails instead of returning "".
func TestGetAMISourceVolumeID_EmptySnapshotSourceVolume(t *testing.T) {
	svc, store := setupProviderImageService(t)
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ebsmetadata.AMI{
		ImageID: "ami-empty", SnapshotID: "snap-empty", ImageOwnerAlias: testAccountID,
	}))
	require.NoError(t, handlers_ec2_snapshot.WriteSnapshotConfig(store, testBucket, "snap-empty",
		&handlers_ec2_snapshot.SnapshotConfig{SnapshotID: "snap-empty"}))

	_, err := svc.GetAMISourceVolumeID(context.Background(), "ami-empty")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
}
