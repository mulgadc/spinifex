package handlers_ec2_volume

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/migrate/ebsmetadatabackfill"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLegacyVolumeConfig writes only a vol-*/config.json, the shape the
// pre-provider embedded engine leaves behind — no ebsmetadata document.
func seedLegacyVolumeConfig(t *testing.T, svc *VolumeServiceImpl, volumeID, accountID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeName: volumeID,
		VolumeSize: 8 * 1024 * 1024 * 1024,
		BlockSize:  4096,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:         volumeID,
				TenantID:         accountID,
				SizeGiB:          8,
				State:            "available",
				AvailabilityZone: "ap-southeast-2a",
				VolumeType:       "gp3",
			},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = svc.store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(svc.bucketName), Key: aws.String(volumeID + "/config.json"), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)
}

// TestDescribeVolumes_Provider_SeesLegacyOnlyVolume is the end-to-end
// assertion behind the ebsmetadata invisibility fix: DescribeVolumes under
// the provider lists ebsmetadata.Store.ListVolumes, which by itself only
// enumerates the ebsmetadata prefix. A volume created before the provider
// switch (or before its backfill migration ran) has no document there, so
// without the legacy fallback wired up it would never appear — this test
// locks that a volume which exists ONLY in the legacy vol-*/config.json
// layout is still visible.
func TestDescribeVolumes_Provider_SeesLegacyOnlyVolume(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	// Mirrors daemon.go's configureEBSProvider wiring for the provider path.
	svc.metadata.SetLegacyVolumeFallback(ebsmetadatabackfill.LegacyVolumeFromLegacyState)

	volumeID := "vol-legacy0000001"
	seedLegacyVolumeConfig(t, svc, volumeID, "acct-legacy")

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "acct-legacy")
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1, "a volume that exists only in the legacy layout must still be visible under the provider")
	assert.Equal(t, volumeID, aws.StringValue(out.Volumes[0].VolumeId))
	assert.Equal(t, int64(8), aws.Int64Value(out.Volumes[0].Size))
}

// TestDescribeVolumes_Provider_WithoutFallback_MissesLegacyOnlyVolume is the
// control: with no fallback wired (the pre-fix state, or a deliberately
// retired fallback), the legacy-only volume is invisible, confirming the
// previous test exercises the fix and not a pre-existing pass.
func TestDescribeVolumes_Provider_WithoutFallback_MissesLegacyOnlyVolume(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))

	seedLegacyVolumeConfig(t, svc, "vol-legacy0000002", "acct-legacy")

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "acct-legacy")
	require.NoError(t, err)
	assert.Empty(t, out.Volumes)
}
