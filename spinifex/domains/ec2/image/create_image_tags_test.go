package image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateImageFromInstance_TagsDescriptionAndEncryption locks what AWS
// answers for a CreateImage given image and snapshot tag specifications: both
// records carry their tags, the snapshot AWS's description, and the image's
// mapping the snapshot's Encrypted, whatever the cluster's own posture is.
func TestCreateImageFromInstance_TagsDescriptionAndEncryption(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unencrypted", true: "encrypted"}[encrypted], func(t *testing.T) {
			svc, _, provider := setupProviderSnapshotImageService(t)
			snapshots := useProvider(svc, provider)
			ctx := context.Background()
			require.False(t, svc.clusterEncryptionEnabled(), "the cluster posture must not be what the mapping reports")

			vol := seedProviderVolume(t, svc, provider, "vol-0562773480b5aa876", 8)
			vol.Encrypted = encrypted
			require.NoError(t, svc.MetadataStore().PutVolume(ctx, vol))

			tags := []*ec2.Tag{
				{Key: aws.String("Name"), Value: aws.String("awsdiff-ami")},
				{Key: aws.String("awsdiff"), Value: aws.String("awsdiff")},
			}
			out, err := svc.CreateImageFromInstance(CreateImageParams{
				Input: &ec2.CreateImageInput{
					InstanceId: aws.String("i-0669dd059c6c9d8cd"),
					Name:       aws.String("awsdiff-ami"),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("image"), Tags: tags},
						{ResourceType: aws.String("snapshot"), Tags: tags},
					},
				},
				RootVolumeID: vol.VolumeID,
			}, testAccountID)
			require.NoError(t, err)
			imageID := aws.StringValue(out.ImageId)

			images, err := svc.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []*string{out.ImageId}}, testAccountID)
			require.NoError(t, err)
			require.Len(t, images.Images, 1)
			image := images.Images[0]
			assert.ElementsMatch(t, tags, image.Tags)
			require.Len(t, image.BlockDeviceMappings, 1)
			ebs := image.BlockDeviceMappings[0].Ebs
			assert.Equal(t, encrypted, aws.BoolValue(ebs.Encrypted))

			listed, err := svc.DescribeImages(ctx, &ec2.DescribeImagesInput{}, testAccountID)
			require.NoError(t, err)
			require.Len(t, listed.Images, 1)
			assert.Equal(t, encrypted, aws.BoolValue(listed.Images[0].BlockDeviceMappings[0].Ebs.Encrypted))

			attr, err := svc.DescribeImageAttribute(ctx, &ec2.DescribeImageAttributeInput{
				ImageId: out.ImageId, Attribute: aws.String(ec2.ImageAttributeNameBlockDeviceMapping),
			}, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, encrypted, aws.BoolValue(attr.BlockDeviceMappings[0].Ebs.Encrypted))

			snaps, err := snapshots.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []*string{ebs.SnapshotId}}, testAccountID)
			require.NoError(t, err)
			require.Len(t, snaps.Snapshots, 1)
			snap := snaps.Snapshots[0]
			assert.ElementsMatch(t, tags, snap.Tags)
			assert.Equal(t, "Created by CreateImage(i-0669dd059c6c9d8cd) for "+imageID, aws.StringValue(snap.Description))
			assert.Equal(t, encrypted, aws.BoolValue(snap.Encrypted))
			assert.Equal(t, vol.VolumeID, aws.StringValue(snap.VolumeId))

			// A copy shares the snapshot's blocks, so it reports the same encryption.
			copied, err := svc.CopyImage(ctx, &ec2.CopyImageInput{
				Name: aws.String("awsdiff-ami-copy"), SourceImageId: out.ImageId, SourceRegion: aws.String("us-east-1"),
			}, testAccountID)
			require.NoError(t, err)
			copyImages, err := svc.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []*string{copied.ImageId}}, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, encrypted, aws.BoolValue(copyImages.Images[0].BlockDeviceMappings[0].Ebs.Encrypted))
		})
	}
}
