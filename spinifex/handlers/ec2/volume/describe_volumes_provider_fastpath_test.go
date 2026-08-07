package handlers_ec2_volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/migrate/ebsmetadatabackfill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDescribeVolumes_Provider_FastPath_SingleID is the regression for the
// +18ms single-volume DescribeVolumes latency: under the provider path, a
// single requested ID must resolve via a direct GetVolume, not by listing
// (and decoding) every volume document in the bucket.
func TestDescribeVolumes_Provider_FastPath_SingleID(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-1")
	require.NoError(t, err)

	out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{vol.VolumeId}}, "acct-1")
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1)
	assert.Equal(t, aws.StringValue(vol.VolumeId), aws.StringValue(out.Volumes[0].VolumeId))
	assert.Equal(t, int64(8), aws.Int64Value(out.Volumes[0].Size))
}

// TestDescribeVolumes_Provider_FastPath_CrossTenantIsNotFound is the most
// important test in this file: a directly-fetched volume belonging to a
// different tenant must never be returned, and must report NotFound exactly
// like an unknown ID — a regression here is a cross-tenant data leak.
func TestDescribeVolumes_Provider_FastPath_CrossTenantIsNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	otherTenantVol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-2")
	require.NoError(t, err)

	out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{otherTenantVol.VolumeId}}, "acct-1")
	require.Error(t, err)
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
	assert.Nil(t, out)
}

// TestDescribeVolumes_Provider_FastPath_UnknownIDIsNotFound covers a
// requested ID with no ebsmetadata document at all.
func TestDescribeVolumes_Provider_FastPath_UnknownIDIsNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String("vol-doesnotexist0")}}, "acct-1")
	require.Error(t, err)
	assert.EqualError(t, err, awserrors.ErrorInvalidVolumeNotFound)
	assert.Nil(t, out)
}

// TestDescribeVolumes_Provider_FastPath_FiltersApply confirms parsedFilters
// still apply to directly-fetched volumes exactly as they do to enumerated
// ones: a requested ID that fails the filter is dropped, not returned.
func TestDescribeVolumes_Provider_FastPath_FiltersApply(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	vol, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-1")
	require.NoError(t, err)

	t.Run("matching filter keeps the volume", func(t *testing.T) {
		out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []*string{vol.VolumeId},
			Filters:   []*ec2.Filter{{Name: aws.String("size"), Values: []*string{aws.String("8")}}},
		}, "acct-1")
		require.NoError(t, err)
		require.Len(t, out.Volumes, 1)
	})

	t.Run("non-matching filter excludes the volume", func(t *testing.T) {
		out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
			VolumeIds: []*string{vol.VolumeId},
			Filters:   []*ec2.Filter{{Name: aws.String("size"), Values: []*string{aws.String("16")}}},
		}, "acct-1")
		require.NoError(t, err)
		assert.Empty(t, out.Volumes)
	})
}

// TestDescribeVolumes_Provider_FastPath_MultipleIDs covers fetching more
// than one directly-requested ID in the same call.
func TestDescribeVolumes_Provider_FastPath_MultipleIDs(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	volA, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-1")
	require.NoError(t, err)
	volB, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(16), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-1")
	require.NoError(t, err)

	out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{volA.VolumeId, volB.VolumeId}}, "acct-1")
	require.NoError(t, err)
	require.Len(t, out.Volumes, 2)

	ids := map[string]bool{}
	for _, v := range out.Volumes {
		ids[aws.StringValue(v.VolumeId)] = true
	}
	assert.True(t, ids[aws.StringValue(volA.VolumeId)])
	assert.True(t, ids[aws.StringValue(volB.VolumeId)])
}

// TestDescribeVolumes_Provider_NoIDs_StillEnumerates locks that the no-IDs
// case keeps using ListVolumes, not the direct-fetch fast path.
func TestDescribeVolumes_Provider_NoIDs_StillEnumerates(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	ctx := context.Background()

	volA, err := svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(8), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-1")
	require.NoError(t, err)
	_, err = svc.CreateVolume(ctx, &ec2.CreateVolumeInput{Size: aws.Int64(16), AvailabilityZone: aws.String("ap-southeast-2a")}, "acct-2")
	require.NoError(t, err)

	out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{}, "acct-1")
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1)
	assert.Equal(t, aws.StringValue(volA.VolumeId), aws.StringValue(out.Volumes[0].VolumeId))
}

// TestDescribeVolumes_Provider_FastPath_ResolvesLegacyOnlyVolume covers
// requirement 4: GetVolume must apply the same legacy-volume fallback as
// ListVolumes, or a pre-provider volume that is visible today (via the
// slow/no-IDs path) would go invisible under the new direct-fetch fast path.
func TestDescribeVolumes_Provider_FastPath_ResolvesLegacyOnlyVolume(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	svc.metadata.SetLegacyVolumeFallback(ebsmetadatabackfill.LegacyVolumeFromLegacyState)
	ctx := context.Background()

	volumeID := "vol-legacy0000003"
	seedLegacyVolumeConfig(t, svc, volumeID, "acct-legacy")

	out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(volumeID)}}, "acct-legacy")
	require.NoError(t, err, "a pre-provider volume must resolve by ID under the provider fast path")
	require.Len(t, out.Volumes, 1)
	assert.Equal(t, volumeID, aws.StringValue(out.Volumes[0].VolumeId))
	assert.Equal(t, int64(8), aws.Int64Value(out.Volumes[0].Size))
}
