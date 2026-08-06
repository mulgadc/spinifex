package handlers_ec2_volume

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/migrate/ebsmetadatabackfill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newProviderVolumeService(t *testing.T) *VolumeServiceImpl {
	t.Helper()
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	return svc
}

// TestGetVolumeConfig_Provider_ResolvesProviderCreatedVolume is the regression
// for the attach 404: a provider-created volume has no vol-*/config.json, so
// reading the legacy key made the daemon report a healthy volume as
// InvalidVolume.NotFound while DescribeVolumes showed it available.
func TestGetVolumeConfig_Provider_ResolvesProviderCreatedVolume(t *testing.T) {
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

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err, "a provider-created volume must resolve without a legacy config.json")
	assert.Equal(t, volumeID, cfg.VolumeMetadata.VolumeID)
	assert.Equal(t, "acct-provider", cfg.VolumeMetadata.TenantID)
	assert.Equal(t, "available", cfg.VolumeMetadata.State)
	assert.Equal(t, "ap-southeast-2a", cfg.VolumeMetadata.AvailabilityZone)
	assert.Equal(t, uint64(8), cfg.VolumeMetadata.SizeGiB)
}

// The daemon's attach validation reads State/AttachedInstance/DeviceName off
// this config to decide VolumeInUse versus idempotent re-attach, so the
// document's attachment fields have to survive the projection.
func TestGetVolumeConfig_Provider_ReflectsAttachmentState(t *testing.T) {
	svc := newProviderVolumeService(t)

	volumeID := "vol-provider00002"
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: "acct-provider", CapacityGiB: 1,
		State: "available", AvailabilityZone: "ap-southeast-2a", VolumeType: "gp3",
	}))

	require.NoError(t, svc.UpdateVolumeState(volumeID, "in-use", "i-0123456789abcdef0", "/dev/sdf"))

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", cfg.VolumeMetadata.State)
	assert.Equal(t, "i-0123456789abcdef0", cfg.VolumeMetadata.AttachedInstance)
	assert.Equal(t, "/dev/sdf", cfg.VolumeMetadata.DeviceName)
}

func TestGetVolumeConfig_Provider_UnknownVolumeIsNotFound(t *testing.T) {
	svc := newProviderVolumeService(t)

	_, err := svc.GetVolumeConfig("vol-doesnotexist0")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// A volume created before the provider switch has only the legacy layout, and
// attach must still resolve it. Routing through ebsmetadata.Store is what puts
// this path behind the legacy fallback; the old direct read never was.
func TestGetVolumeConfig_Provider_ResolvesLegacyOnlyVolume(t *testing.T) {
	svc := newProviderVolumeService(t)
	svc.metadata.SetLegacyVolumeFallback(ebsmetadatabackfill.LegacyVolumeFromLegacyState)

	volumeID := "vol-legacy0000002"
	seedLegacyVolumeConfig(t, svc, volumeID, "acct-legacy")

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err, "a pre-provider volume must remain attachable under the provider")
	assert.Equal(t, volumeID, cfg.VolumeMetadata.VolumeID)
	assert.Equal(t, "acct-legacy", cfg.VolumeMetadata.TenantID)
	assert.Equal(t, "available", cfg.VolumeMetadata.State)
}
