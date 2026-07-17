package handlers_ec2_volume

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeRecordTagsMirror(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-tagmirror0001", "available", "")

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-tagmirror0001")},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testVolAccountID))

	cfg, err := svc.GetVolumeConfig("vol-tagmirror0001")
	require.NoError(t, err)
	assert.Equal(t, "yes", cfg.VolumeMetadata.Tags["keep"])
	assert.Equal(t, "v", cfg.VolumeMetadata.Tags["drop"])

	// Value-mismatched delete is a no-op; matched delete removes.
	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("vol-tagmirror0001")},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testVolAccountID))

	cfg, err = svc.GetVolumeConfig("vol-tagmirror0001")
	require.NoError(t, err)
	assert.Equal(t, "yes", cfg.VolumeMetadata.Tags["keep"])
	_, ok := cfg.VolumeMetadata.Tags["drop"]
	assert.False(t, ok)
}

func TestApplyRecordTags_AttachedVolumePersistsForTagFilterDiscovery(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-taggedattached"
	seedVolume(t, svc, volumeID, "in-use", "i-live0000000000")

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("e2e:run"), Value: aws.String("run-123")}},
	}, testVolAccountID))

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:e2e:run"),
			Values: []*string{aws.String("run-123")},
		}},
	}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1)
	assert.Equal(t, volumeID, aws.StringValue(out.Volumes[0].VolumeId))
}

func TestApplyRecordTags_SurvivesStaleConfigRewrite(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-staleconfigtag"
	seedVolume(t, svc, volumeID, "in-use", "i-live0000000000")
	staleConfig := getStoredConfig(t, store, volumeID)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("sweep"), Value: aws.String("retain")}},
	}, testVolAccountID))

	// Simulate the live nbdkit VB saving its mount-time config after the tag write.
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   bytes.NewReader(staleConfig),
	})
	require.NoError(t, err)

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "retain", cfg.VolumeMetadata.Tags["sweep"])

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:sweep"),
			Values: []*string{aws.String("retain")},
		}},
	}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1)
	assert.Equal(t, volumeID, aws.StringValue(out.Volumes[0].VolumeId))
}

func TestApplyRecordTags_FirstWriteSeedsEmbeddedTags(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-seedembedded"
	seedVolumeWithEmbeddedTags(t, svc, volumeID, map[string]string{"created-with": "volume"})

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("added-later"), Value: aws.String("yes")}},
	}, testVolAccountID))

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"created-with": "volume",
		"added-later":  "yes",
	}, cfg.VolumeMetadata.Tags)
}

func TestRemoveRecordTags_EmptyTagsJSONOverridesEmbeddedTags(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-emptytagsjson"
	seedVolumeWithEmbeddedTags(t, svc, volumeID, map[string]string{"legacy": "tag"})

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("legacy"), Value: aws.String("tag")}},
	}, testVolAccountID))

	tags, found, err := svc.getVolumeTags(context.Background(), volumeID)
	require.NoError(t, err)
	assert.True(t, found, "removing the final tag must leave an authoritative tags.json")
	assert.Empty(t, tags)

	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	assert.Empty(t, cfg.VolumeMetadata.Tags, "present empty tags.json must not fall back to embedded tags")
}

// seedVolumeWithEmbeddedTags creates a legacy or create-time tagged volume with
// no tags.json so tests exercise the migration fallback.
func seedVolumeWithEmbeddedTags(t *testing.T, svc *VolumeServiceImpl, volumeID string, tags map[string]string) {
	t.Helper()
	seedVolume(t, svc, volumeID, "available", "")
	cfg, err := svc.GetVolumeConfig(volumeID)
	require.NoError(t, err)
	cfg.VolumeMetadata.Tags = tags
	require.NoError(t, svc.putVolumeConfig(context.Background(), volumeID, cfg))
}

func TestVolumeRecordTagsMirror_CrossTenantNoMutation(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-tenantguard01", "available", "")

	// A different account tagging this volume must not mutate the record.
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-tenantguard01")},
		Tags:      []*ec2.Tag{{Key: aws.String("evil"), Value: aws.String("1")}},
	}, "999999999999"))

	cfg, err := svc.GetVolumeConfig("vol-tenantguard01")
	require.NoError(t, err)
	assert.Empty(t, cfg.VolumeMetadata.Tags)
}

func TestVolumeRecordTagsMirror_UnownedNoError(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-missing000001"), aws.String("snap-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testVolAccountID))
}
