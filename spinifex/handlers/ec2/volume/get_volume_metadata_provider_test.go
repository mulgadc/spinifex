package handlers_ec2_volume

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newProviderVolumeService(t *testing.T) *VolumeServiceImpl {
	t.Helper()
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	return svc
}

// TestGetVolumeMetadata_Provider_ResolvesProviderCreatedVolume is the regression
// for the attach 404: a provider-created volume has no vol-*/config.json, so
// reading the legacy key made the daemon report a healthy volume as
// InvalidVolume.NotFound while DescribeVolumes showed it available.
func TestGetVolumeMetadata_Provider_ResolvesProviderCreatedVolume(t *testing.T) {
	svc := newProviderVolumeService(t)

	volumeID := "vol-provider00001"
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID:         volumeID,
		TenantID:         "acct-provider",
		CapacityGiB:      8,
		State:            "available",
		CreatedAt:        time.Now(),
		AvailabilityZone: "ap-southeast-2a",
		VolumeType:       "gp3",
		IOPS:             3000,
		Encrypted:        true,
	}))

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err, "a provider-created volume must resolve without a legacy config.json")
	assert.Equal(t, volumeID, meta.VolumeID)
	assert.Equal(t, "acct-provider", meta.TenantID)
	assert.Equal(t, "available", meta.State)
	assert.Equal(t, "ap-southeast-2a", meta.AvailabilityZone)
	assert.Equal(t, uint64(8), meta.CapacityGiB)
}

// The daemon's attach validation reads State/AttachedInstance/DeviceName off
// this config to decide VolumeInUse versus idempotent re-attach, so the
// document's attachment fields have to survive the projection.
func TestGetVolumeMetadata_Provider_ReflectsAttachmentState(t *testing.T) {
	svc := newProviderVolumeService(t)

	volumeID := "vol-provider00002"
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: "acct-provider", CapacityGiB: 1,
		State: "available", AvailabilityZone: "ap-southeast-2a", VolumeType: "gp3",
	}))

	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-0123456789abcdef0", "/dev/sdf"))

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", meta.State)
	assert.Equal(t, "i-0123456789abcdef0", meta.AttachedInstance)
	assert.Equal(t, "/dev/sdf", meta.DeviceName)
}

func TestGetVolumeMetadata_Provider_UnknownVolumeIsNotFound(t *testing.T) {
	svc := newProviderVolumeService(t)

	_, err := svc.GetVolumeMetadata("vol-doesnotexist0")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}
