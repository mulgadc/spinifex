package handlers_ec2_volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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
