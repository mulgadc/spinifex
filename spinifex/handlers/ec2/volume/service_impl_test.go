package handlers_ec2_volume

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/testutil/ebsfake"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestVolumeService(az string) *VolumeServiceImpl {
	return newTestVolumeServiceWithStore(az, objectstore.NewMemoryObjectStore())
}

// seedTestProviderVolume gives the service's provider a volume to operate on.
// A control-plane document alone is not a volume: the provider owns the blocks
// and refuses to expand one it has never allocated.
func seedTestProviderVolume(t *testing.T, svc *VolumeServiceImpl, volumeID string, sizeGiB int64) {
	t.Helper()
	// A fixture with no ID or no size is deliberately unusable and exists to
	// exercise a rejection path, so there is nothing to allocate.
	if svc == nil || svc.provider == nil || volumeID == "" || sizeGiB <= 0 {
		return
	}
	_, err := svc.provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: sizeGiB * 1024 * 1024 * 1024},
		AvailabilityZone: "ap-southeast-2a",
	})
	require.NoError(t, err)
}

func TestCreateVolume_Validation(t *testing.T) {
	tests := []struct {
		name    string
		az      string
		input   *ec2.CreateVolumeInput
		wantErr string
	}{
		{
			name:    "NilInput",
			az:      "ap-southeast-2a",
			input:   nil,
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_Zero",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(0),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_Negative",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(-5),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_TooLarge",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(16385),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_NoSize",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "UnsupportedVolumeType_IO1",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("io1"),
			},
			wantErr: awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "UnsupportedVolumeType_GP2",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("gp2"),
			},
			wantErr: awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "UnsupportedVolumeType_ST1",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("st1"),
			},
			wantErr: awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "Iops_BelowBaseline",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(2999),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "Iops_AboveCeiling",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(16001),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "Iops_AboveRatioForSmallVolume",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(10),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(6000),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "Throughput_BelowBaseline",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Throughput:       aws.Int64(124),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "Throughput_AboveCeiling",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Throughput:       aws.Int64(1001),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "MismatchedAZ",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("us-east-1a"),
			},
			wantErr: awserrors.ErrorInvalidAvailabilityZone,
		},
		{
			name: "EmptyAZ",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String(""),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "NilAZ",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size: aws.Int64(80),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestVolumeService(tt.az)
			_, err := svc.CreateVolume(context.Background(), tt.input, testVolAccountID)
			assert.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

// TestCreateVolume_PassesValidation verifies that valid inputs pass validation
// and only fail at the viperblock/S3 layer (no S3 backend in unit tests).
// ThroughputOmitted_DefaultsToBaseline doubles as a default-value regression
// check: the zero value of the throughput int is 0, which fails the >=125
// range check, so this case only passes if CreateVolume actually assigns the
// 125 baseline when input.Throughput is nil.
func TestCreateVolume_PassesValidation(t *testing.T) {
	tests := []struct {
		name  string
		input *ec2.CreateVolumeInput
	}{
		{
			name: "MinSize",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(1),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "MaxSize",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(16384),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "DefaultsToGP3",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "ExplicitIopsInRange",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(8000),
			},
		},
		{
			name: "ThroughputOmitted_DefaultsToBaseline",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "ExplicitThroughputAtBaseline",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Throughput:       aws.Int64(125),
			},
		},
		{
			name: "ExplicitThroughputAtCeiling",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Throughput:       aws.Int64(1000),
			},
		},
		{
			name: "ExplicitThroughputMidRange",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Throughput:       aws.Int64(500),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			_, err := svc.CreateVolume(context.Background(), tt.input, testVolAccountID)
			if err != nil {
				assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, err.Error())
				assert.NotEqual(t, awserrors.ErrorInvalidAvailabilityZone, err.Error())
			}
		})
	}
}

func TestCreateVolume_UsesInjectedProvider(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.snapshotKV = setupTestVolumeKV(t)

	vol, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size:             aws.Int64(8),
		AvailabilityZone: aws.String("ap-southeast-2a"),
	}, testVolAccountID)
	require.NoError(t, err)
	require.NotNil(t, vol)
	require.NotEmpty(t, vol.VolumeId)

	metadata, err := svc.metadata.GetVolume(context.Background(), testVolAccountID, aws.StringValue(vol.VolumeId))
	require.NoError(t, err)
	assert.Equal(t, testVolAccountID, metadata.TenantID)
	assert.Equal(t, uint64(8), metadata.CapacityGiB)
	assert.Equal(t, "memory://volume/"+aws.StringValue(vol.VolumeId), metadata.ProviderHandle)
	described, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	assert.Equal(t, int64(8), aws.Int64Value(described.Volumes[0].Size))
	require.NoError(t, svc.UpdateVolumeState(testVolAccountID, aws.StringValue(vol.VolumeId), "in-use", "i-123", "/dev/sda1"))
	_, err = svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: vol.VolumeId}, testVolAccountID)
	assert.EqualError(t, err, awserrors.ErrorVolumeInUse)
	require.NoError(t, svc.UpdateVolumeState(testVolAccountID, aws.StringValue(vol.VolumeId), "available", "", ""))
	_, err = svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: vol.VolumeId}, testVolAccountID)
	require.NoError(t, err)
	_, err = svc.metadata.GetVolume(context.Background(), testVolAccountID, aws.StringValue(vol.VolumeId))
	assert.Error(t, err)
}

// TestDescribeVolumes_Provider_FilterAndTenantIsolation covers DescribeVolumes'
// provider-metadata branch: a filter must narrow results, an unknown
// requested volume ID must come back InvalidVolume.NotFound, and one
// tenant must never see another tenant's volumes.
func TestDescribeVolumes_Provider_FilterAndTenantIsolation(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	volA, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)
	_, err = svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(16), AvailabilityZone: aws.String("ap-southeast-2a")}, "000000000002")
	require.NoError(t, err)

	t.Run("tenant isolation", func(t *testing.T) {
		out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{}, testVolAccountID)
		require.NoError(t, err)
		require.Len(t, out.Volumes, 1)
		assert.Equal(t, aws.StringValue(volA.VolumeId), aws.StringValue(out.Volumes[0].VolumeId))
	})

	t.Run("filter match", func(t *testing.T) {
		out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			Filters: []*ec2.Filter{{Name: aws.String("size"), Values: []*string{aws.String("8")}}},
		}, testVolAccountID)
		require.NoError(t, err)
		require.Len(t, out.Volumes, 1)
		assert.Equal(t, aws.StringValue(volA.VolumeId), aws.StringValue(out.Volumes[0].VolumeId))
	})

	t.Run("unknown volume id is InvalidVolume.NotFound", func(t *testing.T) {
		_, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String("vol-does-not-exist")}}, testVolAccountID)
		require.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
	})
}

// TestModifyVolume_Provider covers ModifyVolume's provider-metadata branch:
// a grow succeeds and persists the new capacity, a shrink is rejected
// (grow-only), and an in-use volume is rejected regardless of size.
func TestModifyVolume_Provider(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{OnlineExpansion: true}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)
	volumeID := aws.StringValue(vol.VolumeId)

	t.Run("grow succeeds", func(t *testing.T) {
		out, err := svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: vol.VolumeId, Size: aws.Int64(16)}, testVolAccountID)
		require.NoError(t, err)
		require.NotNil(t, out.VolumeModification)
		assert.Equal(t, int64(8), aws.Int64Value(out.VolumeModification.OriginalSize))
		assert.Equal(t, int64(16), aws.Int64Value(out.VolumeModification.TargetSize))

		meta, err := svc.metadata.GetVolume(ctx, testVolAccountID, volumeID)
		require.NoError(t, err)
		assert.Equal(t, uint64(16), meta.CapacityGiB)
	})

	t.Run("shrink is rejected", func(t *testing.T) {
		_, err := svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: vol.VolumeId, Size: aws.Int64(8)}, testVolAccountID)
		require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
	})

	t.Run("in-use is rejected", func(t *testing.T) {
		require.NoError(t, svc.UpdateVolumeState(testVolAccountID, volumeID, "in-use", "i-123", "/dev/sda1"))
		_, err := svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: vol.VolumeId, Size: aws.Int64(32)}, testVolAccountID)
		require.EqualError(t, err, awserrors.ErrorIncorrectState)
	})

	t.Run("unknown volume id is InvalidVolume.NotFound", func(t *testing.T) {
		_, err := svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: aws.String("vol-does-not-exist"), Size: aws.Int64(32)}, testVolAccountID)
		require.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
	})
}

// TestModifyVolume_Provider_PersistsAndDescribesModification covers 2a/2b:
// a ModifyVolume grow under the provider path must persist its modification
// on the ebsmetadata.Volume document, and a subsequent
// DescribeVolumesModifications (both the fast VolumeIds path and the slow
// list-everything path) must be able to read it back.
func TestModifyVolume_Provider_PersistsAndDescribesModification(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{OnlineExpansion: true}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)
	volumeID := aws.StringValue(vol.VolumeId)

	_, err = svc.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: vol.VolumeId, Size: aws.Int64(16), Iops: aws.Int64(4000)}, testVolAccountID)
	require.NoError(t, err)

	meta, err := svc.metadata.GetVolume(ctx, testVolAccountID, volumeID)
	require.NoError(t, err)
	require.NotNil(t, meta.Modification, "ModifyVolume must persist the modification on the ebsmetadata.Volume document")
	assert.Equal(t, int64(8), meta.Modification.OriginalSize)
	assert.Equal(t, int64(16), meta.Modification.TargetSize)
	assert.Equal(t, int64(4000), meta.Modification.TargetIOPS)

	t.Run("fast path", func(t *testing.T) {
		out, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{
			VolumeIds: []*string{vol.VolumeId},
		}, testVolAccountID)
		require.NoError(t, err)
		require.Len(t, out.VolumesModifications, 1)
		assert.Equal(t, volumeID, aws.StringValue(out.VolumesModifications[0].VolumeId))
		assert.Equal(t, int64(16), aws.Int64Value(out.VolumesModifications[0].TargetSize))
	})

	t.Run("slow path", func(t *testing.T) {
		out, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{}, testVolAccountID)
		require.NoError(t, err)
		require.Len(t, out.VolumesModifications, 1)
		assert.Equal(t, volumeID, aws.StringValue(out.VolumesModifications[0].VolumeId))
	})

	t.Run("unmodified volume has no modification record", func(t *testing.T) {
		other, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
		require.NoError(t, err)
		out, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{
			VolumeIds: []*string{other.VolumeId},
		}, testVolAccountID)
		require.NoError(t, err)
		assert.Empty(t, out.VolumesModifications)
	})
}

// TestDescribeVolumeStatus_Provider covers the DescribeVolumeStatus provider
// gap found during the survey: neither the fast nor the slow path had a
// provider branch, so both silently returned nothing for provider-managed
// volumes.
func TestDescribeVolumeStatus_Provider(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)

	t.Run("fast path", func(t *testing.T) {
		out, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
		require.NoError(t, err)
		require.Len(t, out.VolumeStatuses, 1)
		assert.Equal(t, aws.StringValue(vol.VolumeId), aws.StringValue(out.VolumeStatuses[0].VolumeId))
	})

	t.Run("slow path", func(t *testing.T) {
		out, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{}, testVolAccountID)
		require.NoError(t, err)
		require.Len(t, out.VolumeStatuses, 1)
	})

	t.Run("cross tenant is not found", func(t *testing.T) {
		_, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{VolumeIds: []*string{vol.VolumeId}}, "000000000002")
		require.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
	})
}

// TestCreateVolume_Provider_EncryptedFollowsConfig covers 1b: the Encrypted
// bit on a provider-managed volume must follow config.Viperblock.EncryptionKeyFile,
// the same knob the legacy path uses, and must be visible via both
// CreateVolume's response and a subsequent DescribeVolumes.
func TestCreateVolume_Provider_EncryptedFollowsConfig(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)
	assert.False(t, aws.BoolValue(vol.Encrypted), "no encryption key file configured, so the volume must not report encrypted")

	described, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	assert.False(t, aws.BoolValue(described.Volumes[0].Encrypted))
}

// TestVolumeLeakReaper_Provider covers 2d: the reaper must read attachment
// and tags from ebsmetadata and mark orphaned volumes there when a provider
// is configured, and the mark must be visible via DescribeVolumes.
func TestVolumeLeakReaper_Provider(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)
	volumeID := aws.StringValue(vol.VolumeId)
	require.NoError(t, svc.UpdateVolumeState(testVolAccountID, volumeID, "in-use", "i-gone0000000000", "/dev/sda1"))

	reaper := svc.NewVolumeLeakReaper(func() (map[string]bool, error) {
		return map[string]bool{"i-gone0000000000": true}, nil
	})

	marked, err := reaper.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, marked)

	meta, err := svc.metadata.GetVolume(ctx, testVolAccountID, volumeID)
	require.NoError(t, err)
	assert.NotEmpty(t, meta.Tags[orphanTagKey], "the reaper must mark the provider-managed volume orphaned in ebsmetadata")
	assert.Equal(t, "available", meta.State, "the reaper must reconcile the stale attachment")
	assert.Empty(t, meta.AttachedInstance)

	described, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	assert.NotEmpty(t, filterutil.EC2TagsToMap(described.Volumes[0].Tags)[orphanTagKey],
		"the orphan mark must be visible via DescribeVolumes, which reads ebsmetadata under the provider path")

	// Idempotent: a second sweep re-marks nothing.
	marked, err = reaper.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, marked)
}

// TestApplyRecordTags_Provider_VisibleViaDescribeVolumes covers 2e: a tag
// applied post-create under the provider path must be visible via
// DescribeVolumes, which reads ebsmetadata.Volume.Tags directly (there is no
// tags.json for it to fall back to).
func TestApplyRecordTags_Provider_VisibleViaDescribeVolumes(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, testVolAccountID)
	require.NoError(t, err)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{vol.VolumeId},
		Tags:      []*ec2.Tag{{Key: aws.String("owner"), Value: aws.String("control-plane")}},
	}, testVolAccountID))

	described, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	assert.Equal(t, "control-plane", filterutil.EC2TagsToMap(described.Volumes[0].Tags)["owner"],
		"a tag applied post-create must be visible via DescribeVolumes under the provider path")

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{vol.VolumeId},
		Tags:      []*ec2.Tag{{Key: aws.String("owner"), Value: aws.String("control-plane")}},
	}, testVolAccountID))

	described, err = svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	_, hasOwnerTag := filterutil.EC2TagsToMap(described.Volumes[0].Tags)["owner"]
	assert.False(t, hasOwnerTag, "RemoveRecordTags must remove the tag under the provider path too")

	t.Run("wrong tenant is a no-op", func(t *testing.T) {
		require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
			Resources: []*string{vol.VolumeId},
			Tags:      []*ec2.Tag{{Key: aws.String("owner"), Value: aws.String("intruder")}},
		}, "000000000002"))
		described, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, testVolAccountID)
		require.NoError(t, err)
		_, hasOwnerTag := filterutil.EC2TagsToMap(described.Volumes[0].Tags)["owner"]
		assert.False(t, hasOwnerTag, "a caller who does not own the volume must not be able to tag it")
	})

	t.Run("unknown volume is a no-op", func(t *testing.T) {
		require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
			Resources: []*string{aws.String("vol-does-not-exist")},
			Tags:      []*ec2.Tag{{Key: aws.String("owner"), Value: aws.String("control-plane")}},
		}, testVolAccountID))
	})
}

// prefixFailingObjectStore fails PutObject for any key under failPrefix and
// records the last such key, so a test can recover a randomly generated
// resource ID from the write CreateVolume/CreateSnapshot attempted, without
// needing to predict utils.GenerateResourceID's output up front.
type prefixFailingObjectStore struct {
	objectstore.ObjectStore

	failPrefix   string
	attemptedKey string
}

func (s *prefixFailingObjectStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	key := aws.StringValue(input.Key)
	if strings.HasPrefix(key, s.failPrefix) {
		s.attemptedKey = key
		return nil, errors.New("simulated metadata write failure")
	}
	return s.ObjectStore.PutObject(ctx, input)
}

// TestCreateVolume_Provider_RollbackOnMetadataWriteFailure covers CreateVolume's
// rollback path: when the control-plane metadata write fails after the
// provider has already allocated the volume, CreateVolume must delete the
// just-created provider volume rather than orphan it.
func TestCreateVolume_Provider_RollbackOnMetadataWriteFailure(t *testing.T) {
	store := &prefixFailingObjectStore{ObjectStore: objectstore.NewMemoryObjectStore(), failPrefix: "spinifex/ebsmetadata/v2/volumes/"}
	cfg := &config.Config{AZ: "ap-southeast-2a", Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewVolumeServiceImplWithStore(cfg, store, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)

	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a"),
	}, testVolAccountID)
	require.EqualError(t, err, awserrors.ErrorServerInternal)
	require.NotEmpty(t, store.attemptedKey, "the metadata write must have been attempted")

	volumeID := strings.TrimSuffix(strings.TrimPrefix(store.attemptedKey, store.failPrefix), ".json")
	_, err = provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID})
	require.ErrorIs(t, err, ebsprovider.ErrNotFound, "rollback must delete the just-created provider volume")
}

func TestDeleteVolume_Validation(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteVolumeInput
		wantErr string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.DeleteVolumeInput{},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "NilVolumeId",
			input: &ec2.DeleteVolumeInput{
				VolumeId: nil,
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "EmptyVolumeId",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String(""),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			_, err := svc.DeleteVolume(context.Background(), tt.input, "")
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

func TestDescribeVolumeStatus_NilInputDefaults(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	// nil input is defaulted to empty, then hits the slow path which
	// calls listAllVolumeIDs. With an empty MemoryObjectStore, no
	// volumes are found and an empty result is returned.
	output, err := svc.DescribeVolumeStatus(context.Background(), nil, testVolAccountID)
	require.NoError(t, err)
	assert.Empty(t, output.VolumeStatuses)
}

func TestDescribeVolumeStatus_WithVolumeIDs(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	// When volume IDs are provided, the fast path is taken. With an
	// empty MemoryObjectStore, the volume config is not found and an
	// InvalidVolume.NotFound error is returned.
	_, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-abc123")},
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// newTestVolumeServiceWithStore creates a volume service with a specific memory store.
func newTestVolumeServiceWithStore(az string, store *objectstore.MemoryObjectStore) *VolumeServiceImpl {
	cfg := &config.Config{
		AZ: az,
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			Region:    "ap-southeast-2",
			Host:      fakeS3Host,
			AccessKey: "testkey",
			SecretKey: "testsecret",
		},
		WalDir: "/tmp/test-wal",
	}
	svc := NewVolumeServiceImplWithStore(cfg, store, nil)
	svc.SetEBSProvider(ebsfake.New(store, "test-bucket"))
	return svc
}

// putTestSnapshotMetadata writes snapshot metadata in the store, matching the
// spinifex snapshot service format.
func putTestSnapshotMetadata(t *testing.T, store *objectstore.MemoryObjectStore, snapshotID, ownerID string, sizeGiB int64) {
	t.Helper()
	snapData, err := json.Marshal(snapshotMetadata{
		VolumeID:   "vol-source",
		VolumeSize: sizeGiB,
		OwnerID:    ownerID,
	})
	require.NoError(t, err)

	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader(string(snapData)),
	})
	require.NoError(t, err)
}

func TestCreateVolume_FromSnapshot_PassesValidation(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-test123"
	putTestSnapshotMetadata(t, store, snapshotID, testVolAccountID, 50)

	// CreateVolume from snapshot without explicit size passes validation
	// (fails later at viperblock backend init because no S3 server in tests)
	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, testVolAccountID)
	if err != nil {
		// Should not be a snapshot or validation error - those are the paths we're testing
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	}
}

func TestCreateVolume_FromSnapshot_WithExplicitSize(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-test456"
	putTestSnapshotMetadata(t, store, snapshotID, testVolAccountID, 50)

	// CreateVolume from snapshot with explicit larger size passes validation
	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size:             aws.Int64(100),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, testVolAccountID)
	if err != nil {
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	}
}

func TestCreateVolume_FromSnapshot_NotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String("snap-nonexistent"),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
}

func TestCreateVolume_FromSnapshot_SizeSmallerThanSnapshot(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-sizecheck"
	putTestSnapshotMetadata(t, store, snapshotID, testVolAccountID, 50)

	// Size 10 < snapshot size 50 -- must be rejected
	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

func TestCreateVolume_FromSnapshot_SizeEqualToSnapshot(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-sizeequal"
	putTestSnapshotMetadata(t, store, snapshotID, testVolAccountID, 50)

	// Size == snapshot size should pass validation (may fail at backend init)
	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size:             aws.Int64(50),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, testVolAccountID)
	if err != nil {
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
	}
}

func TestCreateVolume_FromSnapshot_CorruptMetadata(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-corrupt"

	// Put invalid JSON as snapshot metadata
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader("not valid json{{{"),
	})
	require.NoError(t, err)

	_, err = svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, testVolAccountID)
	require.Error(t, err)
}

// assertNoVolumeCreated fails when the store holds any volume config, which is
// the only on-disk trace CreateVolume leaves before the backend is touched.
func assertNoVolumeCreated(t *testing.T, store *objectstore.MemoryObjectStore) {
	t.Helper()
	out, err := store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("test-bucket"),
		Prefix: aws.String("vol-"),
	})
	require.NoError(t, err)
	assert.Empty(t, out.Contents)
}

func TestCreateVolume_FromSnapshot_OtherAccountDenied(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-victim"
	putTestSnapshotMetadata(t, store, snapshotID, "210987654321", 50)

	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
	assertNoVolumeCreated(t, store)
}

// A snapshot recording no owner fails closed. An empty caller is refused
// earlier still, at the boundary, before the snapshot is ever read.
func TestCreateVolume_FromSnapshot_EmptyOwnerDenied(t *testing.T) {
	tests := []struct {
		accountID string
		wantErr   string
	}{
		{accountID: testVolAccountID, wantErr: awserrors.ErrorInvalidSnapshotNotFound},
		{accountID: "", wantErr: awserrors.ErrorAuthFailure},
	}

	for _, tt := range tests {
		t.Run("caller="+tt.accountID, func(t *testing.T) {
			store := objectstore.NewMemoryObjectStore()
			svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

			snapshotID := "snap-legacy"
			putTestSnapshotMetadata(t, store, snapshotID, "", 50)

			_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
				SnapshotId:       aws.String(snapshotID),
			}, tt.accountID)
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
			assertNoVolumeCreated(t, store)
		})
	}
}

// setupTestVolumeKV creates a NATS JetStream test server and returns a KV bucket.
func setupTestVolumeKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket: "spinifex-volume-snapshots",
	})
	require.NoError(t, err)
	return kv
}

func createVolumeInStore(t *testing.T, svc *VolumeServiceImpl, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	createVolumeInStoreWithMeta(t, svc, store, volumeID, ebsmetadata.Volume{
		VolumeID:    volumeID,
		CapacityGiB: 10,
		State:       "available",
	})
}

func TestDeleteVolume_BlockedByKV(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	volumeID := "vol-kvblocked"
	createVolumeInStore(t, svc, store, volumeID)

	// Put a snapshot ref in KV
	data, err := json.Marshal([]string{"snap-001"})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), volumeID, data)
	require.NoError(t, err)

	// DeleteVolume should be blocked
	_, err = svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorVolumeInUse)
}

func TestDeleteVolume_AllowedByKV(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	volumeID := "vol-kvallowed"
	createVolumeInStore(t, svc, store, volumeID)
	seedTestProviderVolume(t, svc, volumeID, 10)

	// No KV entry → delete allowed
	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, testVolAccountID)
	require.NoError(t, err)
}

func TestDeleteVolume_ErrorWhenKVNil(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	// snapshotKV is nil by default

	volumeID := "vol-nokvtest"
	createVolumeInStore(t, svc, store, volumeID)

	// Should fail because snapshotKV is nil
	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorServerInternal)
}

// createVolumeInStoreWithMeta seeds a volume on both sides: the document the
// control plane reads and the provider volume behind it.
func createVolumeInStoreWithMeta(t *testing.T, svc *VolumeServiceImpl, store *objectstore.MemoryObjectStore, volumeID string, meta ebsmetadata.Volume) {
	t.Helper()
	if meta.VolumeID == "" {
		meta.VolumeID = volumeID
	}
	seedVolumeDocument(t, store, meta)
	seedTestProviderVolume(t, svc, meta.VolumeID, safecast.Uint64ToInt64(meta.CapacityGiB))
}

// putRawVolumeDocument writes document bytes verbatim, for cases a typed seed
// cannot express: a corrupt document, or one predating a field.
func putRawVolumeDocument(t *testing.T, store *objectstore.MemoryObjectStore, volumeID, body string) {
	t.Helper()
	key, err := ebsmetadata.VolumeKey(testVolAccountID, volumeID)
	require.NoError(t, err)
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(key),
		Body:   strings.NewReader(body),
	})
	require.NoError(t, err)
}

// --- Group 1: single-volume read and projection tests ---

// volumeByID reads a volume's control-plane document and renders it the way
// DescribeVolumes does, so the read and the projection are covered together.
func volumeByID(t *testing.T, svc *VolumeServiceImpl, volumeID string) *ec2.Volume {
	t.Helper()
	meta, err := svc.GetVolumeMetadata(testVolAccountID, volumeID)
	require.NoError(t, err)
	return metadataVolumeToEC2(meta)
}

func TestGetVolumeByID_FullMetadata(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	now := time.Now()
	meta := ebsmetadata.Volume{
		VolumeID:            "vol-full",
		CapacityGiB:         20,
		State:               "in-use",
		CreatedAt:           now,
		AvailabilityZone:    "ap-southeast-2a",
		VolumeType:          "gp3",
		IOPS:                5000,
		Throughput:          250,
		SnapshotID:          "snap-abc",
		AttachedInstance:    "i-12345",
		DeviceName:          "/dev/nbd0",
		DeleteOnTermination: true,
		AttachedAt:          now,
		Tags:                map[string]string{"Name": "test-vol", "env": "dev"},
	}
	meta.Encrypted = true
	seedVolumeDocument(t, store, meta)

	vol := volumeByID(t, svc, "vol-full")

	assert.Equal(t, "vol-full", *vol.VolumeId)
	assert.Equal(t, int64(20), *vol.Size)
	assert.Equal(t, "in-use", *vol.State)
	assert.Equal(t, "gp3", *vol.VolumeType)
	assert.Equal(t, int64(5000), *vol.Iops)
	assert.Equal(t, int64(250), *vol.Throughput)
	assert.Equal(t, "snap-abc", *vol.SnapshotId)
	assert.True(t, *vol.Encrypted)
	assert.Equal(t, "ap-southeast-2a", *vol.AvailabilityZone)

	// Attachment
	require.Len(t, vol.Attachments, 1)
	att := vol.Attachments[0]
	assert.Equal(t, "i-12345", *att.InstanceId)
	assert.Equal(t, "/dev/nbd0", *att.Device)
	assert.Equal(t, "attached", *att.State)
	assert.True(t, *att.DeleteOnTermination)

	// Tags
	assert.Len(t, vol.Tags, 2)
}

func TestGetVolumeByID_AttachmentDetached(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := ebsmetadata.Volume{
		VolumeID:         "vol-detach",
		CapacityGiB:      10,
		State:            "available",
		AttachedInstance: "i-99999",
		DeviceName:       "/dev/nbd1",
	}
	createVolumeInStoreWithMeta(t, svc, store, "vol-detach", meta)

	vol := volumeByID(t, svc, "vol-detach")

	require.Len(t, vol.Attachments, 1)
	assert.Equal(t, "detached", *vol.Attachments[0].State)
}

func TestGetVolumeByID_DefaultStateAndType(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := ebsmetadata.Volume{
		VolumeID:    "vol-defaults",
		CapacityGiB: 5,
		State:       "",
	}
	createVolumeInStoreWithMeta(t, svc, store, "vol-defaults", meta)

	vol := volumeByID(t, svc, "vol-defaults")

	assert.Equal(t, "available", *vol.State)
	assert.Equal(t, "gp3", *vol.VolumeType)
}

// TestGetVolumeByID_ThroughputOmitted_PreFieldVolume covers a volume written
// before Throughput existed on VolumeMetadata: json.Unmarshal leaves the new
// int field at its zero value, and the projection must omit Throughput from
// the response rather than surface a misleading 0 MiB/s.
func TestGetVolumeByID_ThroughputOmitted_PreFieldVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Simulate a pre-field volume by writing a document with no throughput key.
	putRawVolumeDocument(t, store, "vol-prefield",
		`{"schema_version":2,"volume_id":"vol-prefield","tenant_id":"123456789012","capacity_gib":5,"state":"available","volume_type":"gp3","iops":3000}`)

	vol := volumeByID(t, svc, "vol-prefield")

	assert.Nil(t, vol.Throughput)
}

// A volume with no ID is not representable: the document store refuses to key
// it, so an ID-less volume can never be written and later read back as one.
func TestPutVolume_EmptyVolumeIDRejected(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()

	err := ebsmetadata.NewStore(store, "test-bucket").PutVolume(context.Background(), ebsmetadata.Volume{
		CapacityGiB: 10,
	})
	require.Error(t, err)
}

func TestGetVolumeByID_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	_, err := svc.GetVolumeMetadata(testVolAccountID, "vol-nonexistent")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// --- Group 2: DescribeVolumes tests ---

func TestDescribeVolumes_NilInput(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Seed one volume so slow path has something to find
	createVolumeInStoreWithMeta(t, svc, store, "vol-nil1", ebsmetadata.Volume{
		VolumeID: "vol-nil1", CapacityGiB: 10, State: "available",
	})

	output, err := svc.DescribeVolumes(context.Background(), nil, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 1)
}

func TestDescribeVolumes_EmptyStore(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	output, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, testVolAccountID)
	require.NoError(t, err)
	assert.Empty(t, output.Volumes)
}

func TestDescribeVolumes_SlowPath_MultipleVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-a", "vol-b", "vol-c"} {
		createVolumeInStoreWithMeta(t, svc, store, id, ebsmetadata.Volume{
			VolumeID: id, CapacityGiB: 10, State: "available",
		})
	}

	output, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 3)
}

func TestDescribeVolumes_FastPath_SpecificIDs(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-x", "vol-y", "vol-z"} {
		createVolumeInStoreWithMeta(t, svc, store, id, ebsmetadata.Volume{
			VolumeID: id, CapacityGiB: 10, State: "available",
		})
	}

	output, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-x"), aws.String("vol-z")},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 2)

	ids := map[string]bool{}
	for _, v := range output.Volumes {
		ids[*v.VolumeId] = true
	}
	assert.True(t, ids["vol-x"])
	assert.True(t, ids["vol-z"])
}

func TestDescribeVolumes_FastPath_MixedExistingAndMissing(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-exists", ebsmetadata.Volume{
		VolumeID: "vol-exists", CapacityGiB: 10, State: "available",
	})

	// AWS returns InvalidVolume.NotFound when any requested ID is missing
	_, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-exists"), aws.String("vol-ghost")},
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

func TestDescribeVolumes_FastPath_NilVolumeID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-ok", ebsmetadata.Volume{
		VolumeID: "vol-ok", CapacityGiB: 10, State: "available",
	})

	output, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{nil, aws.String("vol-ok")},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 1)
}

// --- Group 2b: Account scoping tests ---

func TestDescribeVolumes_AccountScoping_SlowPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Create volumes for two different accounts
	createVolumeInStoreWithMeta(t, svc, store, "vol-acctA", ebsmetadata.Volume{
		VolumeID: "vol-acctA", CapacityGiB: 10, State: "available", TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-acctB", ebsmetadata.Volume{
		VolumeID: "vol-acctB", CapacityGiB: 10, State: "available", TenantID: "222222222222",
	})

	// Account A sees only its own volume
	output, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "111111111111")
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, v := range output.Volumes {
		ids[*v.VolumeId] = true
	}
	assert.True(t, ids["vol-acctA"], "Account A should see its own volume")
	assert.False(t, ids["vol-acctB"], "Account A should NOT see Account B's volume")

	// Account B sees only its own volume
	output, err = svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "222222222222")
	require.NoError(t, err)
	ids = map[string]bool{}
	for _, v := range output.Volumes {
		ids[*v.VolumeId] = true
	}
	assert.True(t, ids["vol-acctB"], "Account B should see its own volume")
	assert.False(t, ids["vol-acctA"], "Account B should NOT see Account A's volume")
}

func TestDescribeVolumes_AccountScoping_FastPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-mine", ebsmetadata.Volume{
		VolumeID: "vol-mine", CapacityGiB: 10, State: "available", TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-other", ebsmetadata.Volume{
		VolumeID: "vol-other", CapacityGiB: 10, State: "available", TenantID: "222222222222",
	})

	// Requesting another account's volume by ID returns NotFound
	_, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-other")},
	}, "111111111111")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	// Requesting own volume by ID succeeds
	output, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-mine")},
	}, "111111111111")
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 1)
	assert.Equal(t, "vol-mine", *output.Volumes[0].VolumeId)
}

func TestDeleteVolume_AccountScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = setupTestVolumeKV(t)

	createVolumeInStoreWithMeta(t, svc, store, "vol-owned", ebsmetadata.Volume{
		VolumeID: "vol-owned", CapacityGiB: 10, State: "available", TenantID: "111111111111",
	})

	// Another account cannot delete
	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-owned"),
	}, "222222222222")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	// Owner can delete
	_, err = svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-owned"),
	}, "111111111111")
	require.NoError(t, err)
}

func TestModifyVolume_AccountScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-modify", ebsmetadata.Volume{
		VolumeID: "vol-modify", CapacityGiB: 10, State: "available", TenantID: "111111111111",
	})

	// Another account cannot modify
	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modify"),
		Size:     aws.Int64(20),
	}, "222222222222")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	// Owner can modify
	output, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modify"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)
	assert.Equal(t, int64(20), *output.VolumeModification.TargetSize)
}

func TestDescribeVolumeStatus_AccountScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-statusA", ebsmetadata.Volume{
		VolumeID: "vol-statusA", CapacityGiB: 10, State: "available", TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-statusB", ebsmetadata.Volume{
		VolumeID: "vol-statusB", CapacityGiB: 10, State: "available", TenantID: "222222222222",
	})

	// Slow path: Account A only sees its own volume status
	output, err := svc.DescribeVolumeStatus(context.Background(), nil, "111111111111")
	require.NoError(t, err)
	assert.Len(t, output.VolumeStatuses, 1)
	assert.Equal(t, "vol-statusA", *output.VolumeStatuses[0].VolumeId)

	// Fast path: Account A cannot query Account B's volume status
	_, err = svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-statusB")},
	}, "111111111111")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// --- Group 3: ModifyVolume tests ---

func TestModifyVolume_NilVolumeID(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: nil,
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeIDMalformed, err.Error())
}

func TestModifyVolume_EmptyVolumeID(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String(""),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeIDMalformed, err.Error())
}

func TestModifyVolume_VolumeNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-nonexistent"),
		Size:     aws.Int64(20),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

func TestModifyVolume_ShrinkRejected(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-shrink", ebsmetadata.Volume{
		VolumeID: "vol-shrink", CapacityGiB: 10, State: "available",
	})

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-shrink"),
		Size:     aws.Int64(5),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestModifyVolume_SameSizeRejected(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-same", ebsmetadata.Volume{
		VolumeID: "vol-same", CapacityGiB: 10, State: "available",
	})

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-same"),
		Size:     aws.Int64(10),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestModifyVolume_AttachedInUse(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-inuse", ebsmetadata.Volume{
		VolumeID:         "vol-inuse",
		CapacityGiB:      10,
		State:            "in-use",
		AttachedInstance: "i-12345",
	})

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-inuse"),
		Size:     aws.Int64(20),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectState, err.Error())
}

func TestModifyVolume_SuccessfulGrow(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-grow", ebsmetadata.Volume{
		VolumeID:    "vol-grow",
		CapacityGiB: 10,
		State:       "available",
		VolumeType:  "gp3",
		IOPS:        3000,
	})

	output, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-grow"),
		Size:     aws.Int64(20),
	}, testVolAccountID)
	require.NoError(t, err)

	mod := output.VolumeModification
	assert.Equal(t, "vol-grow", *mod.VolumeId)
	assert.Equal(t, int64(10), *mod.OriginalSize)
	assert.Equal(t, int64(20), *mod.TargetSize)
	assert.Equal(t, "completed", *mod.ModificationState)
	assert.Equal(t, int64(100), *mod.Progress)

	// Verify persisted config
	meta, err := svc.GetVolumeMetadata(testVolAccountID, "vol-grow")
	require.NoError(t, err)
	assert.Equal(t, uint64(20), meta.CapacityGiB)
}

func TestModifyVolume_ModifyTypeAndIOPS(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-typemod", ebsmetadata.Volume{
		VolumeID:    "vol-typemod",
		CapacityGiB: 10,
		State:       "available",
		VolumeType:  "gp3",
		IOPS:        3000,
	})

	output, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId:   aws.String("vol-typemod"),
		Size:       aws.Int64(20),
		VolumeType: aws.String("io1"),
		Iops:       aws.Int64(10000),
	}, testVolAccountID)
	require.NoError(t, err)

	mod := output.VolumeModification
	assert.Equal(t, "gp3", *mod.OriginalVolumeType)
	assert.Equal(t, "io1", *mod.TargetVolumeType)
	assert.Equal(t, int64(3000), *mod.OriginalIops)
	assert.Equal(t, int64(10000), *mod.TargetIops)
}

func TestModifyVolume_AvailableWithAttachment(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Volume attached but state is "available" (stopped instance) -- allowed
	createVolumeInStoreWithMeta(t, svc, store, "vol-stopinst", ebsmetadata.Volume{
		VolumeID:         "vol-stopinst",
		CapacityGiB:      10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})

	output, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-stopinst"),
		Size:     aws.Int64(20),
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(20), *output.VolumeModification.TargetSize)
}

// --- Group 4: UpdateVolumeState tests ---

func TestUpdateVolumeState_AttachVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-attach", ebsmetadata.Volume{
		VolumeID: "vol-attach", CapacityGiB: 10, State: "available",
	})

	err := svc.UpdateVolumeState(testVolAccountID, "vol-attach", "in-use", "i-abc123", "/dev/nbd0")
	require.NoError(t, err)

	meta, err := svc.GetVolumeMetadata(testVolAccountID, "vol-attach")
	require.NoError(t, err)
	assert.Equal(t, "in-use", meta.State)
	assert.Equal(t, "i-abc123", meta.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", meta.DeviceName)
	assert.False(t, meta.AttachedAt.IsZero())
}

func TestUpdateVolumeState_DetachVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-detach2", ebsmetadata.Volume{
		VolumeID:         "vol-detach2",
		CapacityGiB:      10,
		State:            "in-use",
		AttachedInstance: "i-xyz789",
		DeviceName:       "/dev/nbd1",
	})

	err := svc.UpdateVolumeState(testVolAccountID, "vol-detach2", "available", "", "")
	require.NoError(t, err)

	meta, err := svc.GetVolumeMetadata(testVolAccountID, "vol-detach2")
	require.NoError(t, err)
	assert.Equal(t, "available", meta.State)
	assert.Empty(t, meta.AttachedInstance)
	assert.Empty(t, meta.DeviceName)
}

func TestUpdateVolumeState_VolumeNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	err := svc.UpdateVolumeState(testVolAccountID, "vol-missing", "available", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get volume metadata")
}

func TestUpdateVolumeState_PreservesProviderConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vbstate", ebsmetadata.Volume{
		VolumeID: "vol-vbstate", CapacityGiB: 10, State: "available",
	})
	seedProviderConfig(t, store, "vol-vbstate")
	before := getStoredConfig(t, store, "vol-vbstate")

	err := svc.UpdateVolumeState(testVolAccountID, "vol-vbstate", "in-use", "i-preserve", "/dev/nbd0")
	require.NoError(t, err)

	// config.json belongs to the live VB, which rewrites it from its own state.
	// The attachment lives on the document instead, so the bytes must survive.
	assert.Equal(t, string(before), string(getStoredConfig(t, store, "vol-vbstate")),
		"UpdateVolumeState must not rewrite provider-owned config.json")

	readback, err := svc.GetVolumeMetadata(testVolAccountID, "vol-vbstate")
	require.NoError(t, err)
	assert.Equal(t, "in-use", readback.State)
	assert.Equal(t, "i-preserve", readback.AttachedInstance)
}

// --- Group 6: whole-cluster volume listing tests ---

func TestListVolumes_FiltersCorrectly(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-abc", "vol-def"} {
		createVolumeInStoreWithMeta(t, svc, store, id, ebsmetadata.Volume{
			VolumeID: id, CapacityGiB: 10, State: "available",
		})
	}
	// Auxiliary volumes and other resources hold blocks but no document, which
	// is exactly what keeps them out of the listing.
	for _, key := range []string{"vol-abc-efi/config.json", "vol-abc-cloudinit/config.json", "ami-123/metadata.json", "snap-456/metadata.json"} {
		_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String("test-bucket"),
			Key:    aws.String(key),
			Body:   strings.NewReader("{}"),
		})
		require.NoError(t, err)
	}

	volumes, err := svc.metadata.ListVolumes(context.Background())
	require.NoError(t, err)

	// Should only contain vol-abc and vol-def (not efi/cloudinit/ami/snap)
	assert.Len(t, volumes, 2)
	idSet := map[string]bool{}
	for _, v := range volumes {
		idSet[v.VolumeID] = true
	}
	assert.True(t, idSet["vol-abc"])
	assert.True(t, idSet["vol-def"])
}

func TestListVolumes_EmptyBucket(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	volumes, err := svc.metadata.ListVolumes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

func TestListVolumes_SingleDocument(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Seed a single volume to ensure the loop runs
	createVolumeInStoreWithMeta(t, svc, store, "vol-only", ebsmetadata.Volume{
		VolumeID: "vol-only", CapacityGiB: 10, State: "available",
	})

	volumes, err := svc.metadata.ListVolumes(context.Background())
	require.NoError(t, err)
	require.Len(t, volumes, 1)
	assert.Equal(t, "vol-only", volumes[0].VolumeID)
}

// --- Group 7: DeleteVolume remaining tests ---

func TestDeleteVolume_VolumeInUse(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	createVolumeInStoreWithMeta(t, svc, store, "vol-busy", ebsmetadata.Volume{
		VolumeID:         "vol-busy",
		CapacityGiB:      10,
		State:            "in-use",
		AttachedInstance: "i-running",
	})

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-busy"),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorVolumeInUse, err.Error())
}

func TestDeleteVolume_VolumeAttachedButAvailable(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	// State != "available" triggers the check even without "in-use"
	// Actually: the code checks `State != "available" || AttachedInstance != ""`
	// So having AttachedInstance set while state is "available" still triggers VolumeInUse
	createVolumeInStoreWithMeta(t, svc, store, "vol-attached", ebsmetadata.Volume{
		VolumeID:         "vol-attached",
		CapacityGiB:      10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-attached"),
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorVolumeInUse, err.Error())
}

func TestDeleteVolume_EmptyStateUnattachedDeletable(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	// Drift: a detach/terminate left State empty with no attachment. The volume
	// is not in use and must be deletable, not VolumeInUse.
	createVolumeInStoreWithMeta(t, svc, store, "vol-drift", ebsmetadata.Volume{
		VolumeID:    "vol-drift",
		CapacityGiB: 10,
		State:       "",
	})

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-drift"),
	}, testVolAccountID)
	require.NoError(t, err)
}

// TestDeleteVolumeOnTerminate_ClearsAttachmentThenDeletes locks the
// A DeleteOnTermination root volume left attached to a
// stopped instance (Stop's Unmount deliberately never clears a Boot volume's
// AttachedInstance, daemon/vm_adapters.go) hits DeleteVolume's in-use guard
// directly — see TestDeleteVolume_VolumeAttachedButAvailable above.
// DeleteVolumeOnTerminate must clear the stale attachment first (terminate
// implies detach) so the terminate delete actually succeeds instead of
// leaking the volume.
func TestDeleteVolumeOnTerminate_ClearsAttachmentThenDeletes(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	volumeID := "vol-stopped-root"
	createVolumeInStoreWithMeta(t, svc, store, volumeID, ebsmetadata.Volume{
		VolumeID:         volumeID,
		CapacityGiB:      10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})

	err := svc.DeleteVolumeOnTerminate(context.Background(), volumeID, testVolAccountID)
	require.NoError(t, err, "terminate implies detach: a stale attachment must not block the terminate delete")

	_, err = svc.GetVolumeMetadata(testVolAccountID, volumeID)
	require.Error(t, err, "the volume must actually be deleted, not merely detached")
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidVolumeNotFound)
}

// TestDeleteVolumeOnTerminate_AlreadyGoneMetadataIsSuccess verifies GC
// re-driving the volumes dependent on an instance whose volume metadata
// document is already deleted is treated as success, not a hard failure that
// pins the reaper backoff at its cap forever.
func TestDeleteVolumeOnTerminate_AlreadyGoneMetadataIsSuccess(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	// No metadata doc is ever written for this volume ID.
	err := svc.DeleteVolumeOnTerminate(context.Background(), "vol-already-gone", testVolAccountID)
	require.NoError(t, err, "a volume with no metadata document is already gone, not a teardown failure")
}

// TestDetachVolumeOnTerminate_AlreadyGoneMetadataIsSuccess mirrors
// TestDeleteVolumeOnTerminate_AlreadyGoneMetadataIsSuccess for the
// DeleteOnTermination=false path.
func TestDetachVolumeOnTerminate_AlreadyGoneMetadataIsSuccess(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	err := svc.DetachVolumeOnTerminate(context.Background(), "vol-already-gone", testVolAccountID)
	require.NoError(t, err, "a volume with no metadata document is already gone, not a teardown failure")
}

// TestDeleteVolumeOnTerminate_SurfacesDeleteFailure verifies a DeleteVolume
// failure downstream of the attachment clear is returned, not swallowed — the
// caller (deleteInstanceVolumes / instanceCleanerAdapter.DeleteVolumes) relies
// on this to mark Teardown[TeardownVolumes] failed rather than silently
// dropping the leak.
func TestDeleteVolumeOnTerminate_SurfacesDeleteFailure(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	volumeID := "vol-snapshotted-root"
	createVolumeInStoreWithMeta(t, svc, store, volumeID, ebsmetadata.Volume{
		VolumeID:         volumeID,
		CapacityGiB:      10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})
	// A dependent snapshot forces DeleteVolume to fail after the attachment
	// clear has already succeeded.
	snapData, err := json.Marshal([]string{"snap-001"})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), volumeID, snapData)
	require.NoError(t, err)

	err = svc.DeleteVolumeOnTerminate(context.Background(), volumeID, testVolAccountID)
	require.Error(t, err, "a DeleteVolume failure must be surfaced, not swallowed")
	assert.Contains(t, err.Error(), awserrors.ErrorVolumeInUse)

	meta, getErr := svc.GetVolumeMetadata(testVolAccountID, volumeID)
	require.NoError(t, getErr, "the volume must still exist after a failed delete")
	assert.Empty(t, meta.AttachedInstance, "the attachment clear runs before delete and is not rolled back on a later delete failure")
}

func TestDescribeVolumes_EmptyStateDerivedFromAttachment(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-empty-unattached", "", "")
	seedVolume(t, svc, "vol-empty-attached", "", "i-attached00000000")

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-empty-unattached"), aws.String("vol-empty-attached")},
	}, testVolAccountID)
	require.NoError(t, err)

	got := map[string]string{}
	for _, v := range out.Volumes {
		got[aws.StringValue(v.VolumeId)] = aws.StringValue(v.State)
	}
	assert.Equal(t, "available", got["vol-empty-unattached"], "empty state + no attachment renders available")
	assert.Equal(t, "in-use", got["vol-empty-attached"], "empty state + attachment must not be masked as available")
}

func TestUpdateVolumeState_EmptyUnattachedNormalizesToAvailable(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-norm", "in-use", "i-x")

	// A detach writeback that clears the attachment without a state must not
	// strand the volume with an empty State.
	require.NoError(t, svc.UpdateVolumeState(testVolAccountID, "vol-norm", "", "", ""))
	meta, err := svc.GetVolumeMetadata(testVolAccountID, "vol-norm")
	require.NoError(t, err)
	assert.Equal(t, "available", meta.State)
	assert.Empty(t, meta.AttachedInstance)
}

func TestDescribeVolumeStatus_SlowPath_WithVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-s1", "vol-s2"} {
		createVolumeInStoreWithMeta(t, svc, store, id, ebsmetadata.Volume{
			VolumeID:         id,
			CapacityGiB:      10,
			State:            "available",
			AvailabilityZone: "ap-southeast-2a",
		})
	}

	output, err := svc.DescribeVolumeStatus(context.Background(), nil, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, output.VolumeStatuses, 2)

	for _, item := range output.VolumeStatuses {
		assert.Equal(t, "ok", *item.VolumeStatus.Status)
		assert.Equal(t, "ap-southeast-2a", *item.AvailabilityZone)
		assert.Len(t, item.VolumeStatus.Details, 2)
	}
}

func TestDescribeVolumeStatus_FastPath_WithVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-status1", ebsmetadata.Volume{
		VolumeID:         "vol-status1",
		CapacityGiB:      10,
		State:            "in-use",
		AvailabilityZone: "ap-southeast-2a",
	})

	output, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-status1")},
	}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, output.VolumeStatuses, 1)
	assert.Equal(t, "vol-status1", *output.VolumeStatuses[0].VolumeId)
	assert.Equal(t, "ok", *output.VolumeStatuses[0].VolumeStatus.Status)
}

// --- DescribeVolumes filter tests ---

func TestDescribeVolumes_FilterByStatus(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-avail", ebsmetadata.Volume{
		VolumeID: "vol-avail", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-inuse", ebsmetadata.Volume{
		VolumeID: "vol-inuse", CapacityGiB: 20, State: "in-use", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-avail", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByVolumeType(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-gp3", ebsmetadata.Volume{
		VolumeID: "vol-gp3", CapacityGiB: 10, State: "available", VolumeType: "gp3", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-io1", ebsmetadata.Volume{
		VolumeID: "vol-io1", CapacityGiB: 10, State: "available", VolumeType: "io1", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-type"), Values: []*string{aws.String("gp3")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-gp3", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterBySize(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-small", ebsmetadata.Volume{
		VolumeID: "vol-small", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-big", ebsmetadata.Volume{
		VolumeID: "vol-big", CapacityGiB: 100, State: "available", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("size"), Values: []*string{aws.String("100")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-big", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByAttachmentInstanceId(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-att", ebsmetadata.Volume{
		VolumeID: "vol-att", CapacityGiB: 10, State: "in-use",
		AttachedInstance: "i-12345", DeviceName: "/dev/nbd0", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-free", ebsmetadata.Volume{
		VolumeID: "vol-free", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []*string{aws.String("i-12345")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-att", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByAttachmentDevice(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-nbd0", ebsmetadata.Volume{
		VolumeID: "vol-nbd0", CapacityGiB: 10, State: "in-use",
		AttachedInstance: "i-12345", DeviceName: "/dev/nbd0", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-nbd1", ebsmetadata.Volume{
		VolumeID: "vol-nbd1", CapacityGiB: 10, State: "in-use",
		AttachedInstance: "i-12345", DeviceName: "/dev/nbd1", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.device"), Values: []*string{aws.String("/dev/nbd1")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-nbd1", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByAZ(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-az1", ebsmetadata.Volume{
		VolumeID: "vol-az1", CapacityGiB: 10, State: "available",
		AvailabilityZone: "ap-southeast-2a", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-az2", ebsmetadata.Volume{
		VolumeID: "vol-az2", CapacityGiB: 10, State: "available",
		AvailabilityZone: "ap-southeast-2b", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-az1", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterMultipleValues_OR(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-avail", ebsmetadata.Volume{
		VolumeID: "vol-avail", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-inuse", ebsmetadata.Volume{
		VolumeID: "vol-inuse", CapacityGiB: 10, State: "in-use",
		AttachedInstance: "i-1", DeviceName: "/dev/nbd0", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-del", ebsmetadata.Volume{
		VolumeID: "vol-del", CapacityGiB: 10, State: "deleted", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available"), aws.String("in-use")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 2)
}

func TestDescribeVolumes_FilterMultipleFilters_AND(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-match", ebsmetadata.Volume{
		VolumeID: "vol-match", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-nomatch", ebsmetadata.Volume{
		VolumeID: "vol-nomatch", CapacityGiB: 10, State: "in-use",
		VolumeType: "gp3", AttachedInstance: "i-1", DeviceName: "/dev/nbd0", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
			{Name: aws.String("volume-type"), Values: []*string{aws.String("gp3")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-match", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterUnknownName_Error(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	_, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("val")}},
		},
	}, "000000000001")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeVolumes_FilterNoResults(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-one", ebsmetadata.Volume{
		VolumeID: "vol-one", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("deleted")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Empty(t, out.Volumes)
}

func TestDescribeVolumes_FilterNoFilters(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-a", ebsmetadata.Volume{
		VolumeID: "vol-a", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-b", ebsmetadata.Volume{
		VolumeID: "vol-b", CapacityGiB: 20, State: "available", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 2)
}

func TestDescribeVolumes_FilterWildcard(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-az1", ebsmetadata.Volume{
		VolumeID: "vol-az1", CapacityGiB: 10, State: "available",
		AvailabilityZone: "ap-southeast-2a", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-az2", ebsmetadata.Volume{
		VolumeID: "vol-az2", CapacityGiB: 10, State: "available",
		AvailabilityZone: "us-east-1a", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-*")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-az1", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByTag(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-tagged", ebsmetadata.Volume{
		VolumeID: "vol-tagged", CapacityGiB: 10, State: "available", TenantID: "000000000001",
		Tags: map[string]string{"Environment": "prod"},
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-untagged", ebsmetadata.Volume{
		VolumeID: "vol-untagged", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Environment"), Values: []*string{aws.String("prod")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-tagged", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterWithVolumeIds(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-a", ebsmetadata.Volume{
		VolumeID: "vol-a", CapacityGiB: 10, State: "available", TenantID: "000000000001",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-b", ebsmetadata.Volume{
		VolumeID: "vol-b", CapacityGiB: 10, State: "in-use",
		AttachedInstance: "i-1", DeviceName: "/dev/nbd0", TenantID: "000000000001",
	})

	// Request both by ID but filter to only available
	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-a"), aws.String("vol-b")},
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
		},
	}, "000000000001")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-a", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumeStatus_FilterByVolumeId(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vs1", ebsmetadata.Volume{
		VolumeID: "vol-vs1", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-vs2", ebsmetadata.Volume{
		VolumeID: "vol-vs2", CapacityGiB: 20, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vs1")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, out.VolumeStatuses, 1)
	assert.Equal(t, "vol-vs1", *out.VolumeStatuses[0].VolumeId)
}

func TestDescribeVolumeStatus_FilterByStatus(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vss1", ebsmetadata.Volume{
		VolumeID: "vol-vss1", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	// Status is always "ok" in Spinifex
	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-status.status"), Values: []*string{aws.String("ok")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)

	out, err = svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-status.status"), Values: []*string{aws.String("impaired")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterByAZ(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vsaz", ebsmetadata.Volume{
		VolumeID: "vol-vsaz", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)

	out, err = svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("us-east-1a")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterMultipleValues_OR(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vsor1", ebsmetadata.Volume{
		VolumeID: "vol-vsor1", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-vsor2", ebsmetadata.Volume{
		VolumeID: "vol-vsor2", CapacityGiB: 20, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-vsor3", ebsmetadata.Volume{
		VolumeID: "vol-vsor3", CapacityGiB: 30, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vsor1"), aws.String("vol-vsor3")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 2)
}

func TestDescribeVolumeStatus_FilterMultipleFilters_AND(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vsand", ebsmetadata.Volume{
		VolumeID: "vol-vsand", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	// Both match
	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vsand")}},
			{Name: aws.String("volume-status.status"), Values: []*string{aws.String("ok")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)

	// Mismatch
	out, err = svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vsand")}},
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("us-east-1a")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterUnknownName_Error(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	_, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testVolAccountID)
	assert.Error(t, err)
}

func TestDescribeVolumeStatus_FilterWildcard(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vswild", ebsmetadata.Volume{
		VolumeID: "vol-vswild", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vswild*")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)
}

func TestDescribeVolumeStatus_FilterNoResults(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vsnr", ebsmetadata.Volume{
		VolumeID: "vol-vsnr", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-nonexistent")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterWithVolumeIds(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-vsf1", ebsmetadata.Volume{
		VolumeID: "vol-vsf1", CapacityGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-vsf2", ebsmetadata.Volume{
		VolumeID: "vol-vsf2", CapacityGiB: 20, State: "available", AvailabilityZone: "us-east-1a",
	})

	// Fast path with VolumeIds + filter: should apply filter to requested IDs
	out, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-vsf1"), aws.String("vol-vsf2")},
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)
	assert.Equal(t, "vol-vsf1", *out.VolumeStatuses[0].VolumeId)
}

// --- Group: DescribeVolumesModifications tests ---

// TestDescribeVolumesModifications_RoundTrip proves ModifyVolume persists the
// modification record into meta.Modification AND that DescribeVolumesModifications
// reads it back. Guards the load-bearing wiring between the two APIs.
func TestDescribeVolumesModifications_RoundTrip(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-rt", ebsmetadata.Volume{
		VolumeID: "vol-rt", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-rt"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)

	// Confirm Modification was persisted on cfg.
	meta, err := svc.GetVolumeMetadata("111111111111", "vol-rt")
	require.NoError(t, err)
	require.NotNil(t, meta.Modification)
	assert.Equal(t, int64(10), meta.Modification.OriginalSize)
	assert.Equal(t, int64(20), meta.Modification.TargetSize)

	out, err := svc.DescribeVolumesModifications(context.Background(), &ec2.DescribeVolumesModificationsInput{
		VolumeIds: []*string{aws.String("vol-rt")},
	}, "111111111111")
	require.NoError(t, err)
	require.Len(t, out.VolumesModifications, 1)
	mod := out.VolumesModifications[0]
	assert.Equal(t, "vol-rt", *mod.VolumeId)
	assert.Equal(t, "completed", *mod.ModificationState)
	assert.Equal(t, int64(100), *mod.Progress)
	assert.Equal(t, int64(10), *mod.OriginalSize)
	assert.Equal(t, int64(20), *mod.TargetSize)
}

// TestDescribeVolumesModifications_OverwriteSemantics guards the single-record
// storage design: a second ModifyVolume must overwrite, never append. The
// second call's OriginalSize must equal the first call's TargetSize.
func TestDescribeVolumesModifications_OverwriteSemantics(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-ow", ebsmetadata.Volume{
		VolumeID: "vol-ow", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-ow"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)

	_, err = svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-ow"),
		Size:     aws.Int64(40),
	}, "111111111111")
	require.NoError(t, err)

	out, err := svc.DescribeVolumesModifications(context.Background(), nil, "111111111111")
	require.NoError(t, err)
	require.Len(t, out.VolumesModifications, 1, "expected single record after two ModifyVolume calls")
	mod := out.VolumesModifications[0]
	assert.Equal(t, int64(20), *mod.OriginalSize, "second modification's OriginalSize must equal first's TargetSize")
	assert.Equal(t, int64(40), *mod.TargetSize)
}

// TestDescribeVolumesModifications_CrossTenantFastPath guards tenant isolation:
// querying another tenant's volume by explicit ID must return InvalidVolume.NotFound,
// not leak the modification record.
func TestDescribeVolumesModifications_CrossTenantFastPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-tenantA", ebsmetadata.Volume{
		VolumeID: "vol-tenantA", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-tenantA"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)

	_, err = svc.DescribeVolumesModifications(context.Background(), &ec2.DescribeVolumesModificationsInput{
		VolumeIds: []*string{aws.String("vol-tenantA")},
	}, "222222222222")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestDescribeVolumesModifications_SlowPathScoping covers the list-all path:
// unmodified volumes and cross-tenant volumes must both be silently omitted.
func TestDescribeVolumesModifications_SlowPathScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-modA", ebsmetadata.Volume{
		VolumeID: "vol-modA", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-unmodA", ebsmetadata.Volume{
		VolumeID: "vol-unmodA", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-modB", ebsmetadata.Volume{
		VolumeID: "vol-modB", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "222222222222",
	})

	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modA"), Size: aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)
	_, err = svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modB"), Size: aws.Int64(30),
	}, "222222222222")
	require.NoError(t, err)

	out, err := svc.DescribeVolumesModifications(context.Background(), nil, "111111111111")
	require.NoError(t, err)
	require.Len(t, out.VolumesModifications, 1)
	assert.Equal(t, "vol-modA", *out.VolumesModifications[0].VolumeId)
}

// TestDescribeVolumesModifications_FilterMatching exercises the filter switch
// for the field types whose comparison logic differs (string equality, numeric
// stringification, and the volume-id pre-filter on the slow path).
func TestDescribeVolumesModifications_FilterMatching(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, svc, store, "vol-fa", ebsmetadata.Volume{
		VolumeID: "vol-fa", CapacityGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, svc, store, "vol-fb", ebsmetadata.Volume{
		VolumeID: "vol-fb", CapacityGiB: 50, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	_, err := svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-fa"), Size: aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)
	_, err = svc.ModifyVolume(context.Background(), &ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-fb"), Size: aws.Int64(100), VolumeType: aws.String("io1"), Iops: aws.Int64(8000),
	}, "111111111111")
	require.NoError(t, err)

	tests := []struct {
		name      string
		filter    *ec2.Filter
		wantIDs   []string
		wantEmpty bool
	}{
		{
			name:    "ByVolumeID",
			filter:  &ec2.Filter{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-fa")}},
			wantIDs: []string{"vol-fa"},
		},
		{
			name:    "ByModificationState",
			filter:  &ec2.Filter{Name: aws.String("modification-state"), Values: []*string{aws.String("completed")}},
			wantIDs: []string{"vol-fa", "vol-fb"},
		},
		{
			name:      "ByModificationStateNoMatch",
			filter:    &ec2.Filter{Name: aws.String("modification-state"), Values: []*string{aws.String("failed")}},
			wantEmpty: true,
		},
		{
			name:    "ByTargetSizeNumeric",
			filter:  &ec2.Filter{Name: aws.String("target-size"), Values: []*string{aws.String("20")}},
			wantIDs: []string{"vol-fa"},
		},
		{
			name:    "ByTargetVolumeType",
			filter:  &ec2.Filter{Name: aws.String("target-volume-type"), Values: []*string{aws.String("io1")}},
			wantIDs: []string{"vol-fb"},
		},
		{
			name:      "TagFilterAlwaysEmpty",
			filter:    &ec2.Filter{Name: aws.String("tag:env"), Values: []*string{aws.String("prod")}},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := svc.DescribeVolumesModifications(context.Background(), &ec2.DescribeVolumesModificationsInput{
				Filters: []*ec2.Filter{tt.filter},
			}, "111111111111")
			require.NoError(t, err)
			if tt.wantEmpty {
				assert.Empty(t, out.VolumesModifications)
				return
			}
			got := make([]string, 0, len(out.VolumesModifications))
			for _, m := range out.VolumesModifications {
				got = append(got, *m.VolumeId)
			}
			assert.ElementsMatch(t, tt.wantIDs, got)
		})
	}
}

// TestDescribeVolumesModifications_UnknownVolumeIDFastPath covers the goroutine
// GetVolumeConfig-error branch and the caller's per-result error scan.
func TestDescribeVolumesModifications_UnknownVolumeIDFastPath(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.DescribeVolumesModifications(context.Background(), &ec2.DescribeVolumesModificationsInput{
		VolumeIds: []*string{aws.String("vol-doesnotexist")},
	}, "111111111111")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestDescribeVolumesModifications_UnknownFilter proves the filter validator is
// wired up: an unknown filter name must fail with InvalidParameterValue.
func TestDescribeVolumesModifications_UnknownFilter(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.DescribeVolumesModifications(context.Background(), &ec2.DescribeVolumesModificationsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("not-a-real-filter"), Values: []*string{aws.String("x")}},
		},
	}, testVolAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
