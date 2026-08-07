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

	meta, err := svc.GetVolumeMetadata("vol-tagmirror0001")
	require.NoError(t, err)
	assert.Equal(t, "yes", meta.Tags["keep"])
	assert.Equal(t, "v", meta.Tags["drop"])

	// Value-mismatched delete is a no-op; matched delete removes.
	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String("vol-tagmirror0001")},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testVolAccountID))

	meta, err = svc.GetVolumeMetadata("vol-tagmirror0001")
	require.NoError(t, err)
	assert.Equal(t, "yes", meta.Tags["keep"])
	_, ok := meta.Tags["drop"]
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

// A tag written against a pre-migration volume must survive the live nbdkit VB
// rewriting config.json from its own mount-time state: the tag lives in the
// control-plane document, which that writer does not own.
func TestApplyRecordTags_SurvivesStaleConfigRewrite(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-staleconfigtag"
	seedLegacyVolumeConfig(t, svc, volumeID, testVolAccountID)
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

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Equal(t, "retain", meta.Tags["sweep"])

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

// The first control-plane tag write on a pre-migration volume must merge with
// the tags embedded in its legacy config.json, not replace them.
func TestApplyRecordTags_FirstWriteSeedsEmbeddedTags(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-seedembedded"
	seedLegacyVolumeConfigWithTags(t, svc, volumeID, testVolAccountID, map[string]string{"created-with": "volume"})

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("added-later"), Value: aws.String("yes")}},
	}, testVolAccountID))

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"created-with": "volume",
		"added-later":  "yes",
	}, meta.Tags)
}

// Removing the last tag must leave an authoritative empty tag set on the
// document rather than falling back to the tags still embedded in the legacy
// config.json, which would resurrect the deleted tag.
func TestRemoveRecordTags_EmptyDocumentTagsOverrideEmbeddedTags(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-emptytagsjson"
	seedLegacyVolumeConfigWithTags(t, svc, volumeID, testVolAccountID, map[string]string{"legacy": "tag"})

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("legacy"), Value: aws.String("tag")}},
	}, testVolAccountID))

	meta, err := svc.GetVolumeMetadata(volumeID)
	require.NoError(t, err)
	assert.Empty(t, meta.Tags, "an empty document tag set must not fall back to embedded tags")
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

	meta, err := svc.GetVolumeMetadata("vol-tenantguard01")
	require.NoError(t, err)
	assert.Empty(t, meta.Tags)
}

func TestVolumeRecordTagsMirror_UnownedNoError(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("vol-missing000001"), aws.String("snap-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testVolAccountID))
}
