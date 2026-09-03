package handlers_ec2_image

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putSourceVolumeAMI registers an AMI document plus, when volumeID is set, the
// snapshot document recording which volume the snapshot was taken from. The
// document is keyed under the account the AMI's owner alias derives to, which
// is what GetAMISourceVolumeID has to reproduce to find it.
func putSourceVolumeAMI(t *testing.T, svc *ImageServiceImpl, ami ebsmetadata.AMI, volumeID string) {
	t.Helper()
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ami))
	if volumeID == "" {
		return
	}
	owner, _ := snapshotAccountForAMI(ami)
	require.NoError(t, svc.MetadataStore().PutSnapshot(context.Background(), ebsmetadata.Snapshot{
		SnapshotID: ami.SnapshotID, VolumeID: volumeID, OwnerID: owner,
	}))
}

// TestGetAMISourceVolumeID_ReadsSnapshotMetadata locks the normal case: the
// source volume comes from the snapshot's own document.
func TestGetAMISourceVolumeID_ReadsSnapshotMetadata(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, ebsmetadata.AMI{
		ImageID: "ami-src01", SnapshotID: "snap-src01", ImageOwnerAlias: testAccountID,
	}, "vol-origin")

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-src01")
	require.NoError(t, err)
	assert.Equal(t, "vol-origin", got)
}

// TestGetAMISourceVolumeID_BundledSystemAMI locks the fallback for bundled
// system AMIs, whose snapshot is named after the AMI and carries no metadata.json.
func TestGetAMISourceVolumeID_BundledSystemAMI(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, ebsmetadata.AMI{
		ImageID: "ami-sys01", SnapshotID: "snap-ami-sys01", ImageOwnerAlias: "system",
	}, "")

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-sys01")
	require.NoError(t, err)
	assert.Equal(t, "ami-sys01", got, "the bundled AMI's snapshot reads chunks from a volume named after the AMI")
}

// TestGetAMISourceVolumeID_BundledSystemAMI_LogsFallbackWarning locks that the
// bundled-system fallback is observable: it masks a missing control-plane
// document, so a caller relying on it (e.g. a stale catalog import predating
// this fix) must be able to spot it in the logs rather than launch silently.
func TestGetAMISourceVolumeID_BundledSystemAMI_LogsFallbackWarning(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, ebsmetadata.AMI{
		ImageID: "ami-sys02", SnapshotID: "snap-ami-sys02", ImageOwnerAlias: "system",
	}, "")

	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-sys02")
	require.NoError(t, err)
	assert.Equal(t, "ami-sys02", got)

	logs := buf.String()
	assert.Contains(t, logs, "level=WARN", "the fallback masking a missing snapshot document must be visible")
	assert.Contains(t, logs, "ami-sys02")
}

// TestGetAMISourceVolumeID_ImportedSystemAMI locks that a system AMI whose
// snapshot document exists under the global account resolves through it rather
// than taking the bundled fallback. The bundled path logs a warn; a wrongly
// derived account here would silently return an image ID where a volume ID
// belongs, and the launch would clone the wrong blocks.
func TestGetAMISourceVolumeID_ImportedSystemAMI(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ebsmetadata.AMI{
		ImageID: "ami-sysimp01", SnapshotID: "snap-sysimp01", ImageOwnerAlias: "system",
	}))
	// The owner is spelled out rather than derived, reproducing what
	// registerImportedAMISnapshot writes. Deriving it here would let the
	// fixture follow the read path and pin nothing.
	require.NoError(t, svc.MetadataStore().PutSnapshot(context.Background(), ebsmetadata.Snapshot{
		SnapshotID: "snap-sysimp01", VolumeID: "vol-imported", OwnerID: utils.GlobalAccountID,
	}))

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-sysimp01")
	require.NoError(t, err)
	assert.Equal(t, "vol-imported", got, "an imported system AMI must resolve its recorded source volume, not fall back")
}

// TestGetAMISourceVolumeID_EmptyOwnerAliasDoesNotFallBack locks that an AMI with
// no owner alias is treated as corrupt rather than as a system image. Deriving
// it to the global account and letting the bundled fallback fire would return
// the image ID where a volume ID belongs, and the launch would clone holes.
func TestGetAMISourceVolumeID_EmptyOwnerAliasDoesNotFallBack(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, ebsmetadata.AMI{
		ImageID: "ami-noalias", SnapshotID: "snap-noalias", ImageOwnerAlias: "",
	}, "")

	got, err := svc.GetAMISourceVolumeID(context.Background(), "ami-noalias")
	require.Error(t, err)
	assert.Empty(t, got, "a corrupt AMI must not resolve to its own image ID")
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
}

// TestGetAMISourceVolumeID_AccountAMIMissingSnapshotMetadata locks that the
// bundled fallback does not apply to account-owned AMIs.
func TestGetAMISourceVolumeID_AccountAMIMissingSnapshotMetadata(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, ebsmetadata.AMI{
		ImageID: "ami-acct01", SnapshotID: "snap-acct01", ImageOwnerAlias: testAccountID,
	}, "")

	_, err := svc.GetAMISourceVolumeID(context.Background(), "ami-acct01")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
}

// TestGetAMISourceVolumeID_AMIWithoutSnapshot locks that an AMI carrying no
// snapshot reference is reported as not found rather than resolving to "".
func TestGetAMISourceVolumeID_AMIWithoutSnapshot(t *testing.T) {
	svc, _ := setupProviderImageService(t)
	putSourceVolumeAMI(t, svc, ebsmetadata.AMI{
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
	svc, _ := setupProviderImageService(t)
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ebsmetadata.AMI{
		ImageID: "ami-empty", SnapshotID: "snap-empty", ImageOwnerAlias: testAccountID,
	}))
	require.NoError(t, svc.MetadataStore().PutSnapshot(context.Background(), ebsmetadata.Snapshot{
		SnapshotID: "snap-empty", OwnerID: testAccountID,
	}))

	_, err := svc.GetAMISourceVolumeID(context.Background(), "ami-empty")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
}
