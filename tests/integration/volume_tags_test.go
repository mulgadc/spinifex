//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDescribeTags_ReturnsCreateVolumeCreationTags creates a tagged volume and
// reads its tags back through the public EC2 API: the SDK client signs a
// query-protocol request to the real gateway, which reaches the real volume
// and tag services over NATS.
func TestDescribeTags_ReturnsCreateVolumeCreationTags(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw, WithVolumeTagReader(StartVolumeDaemonLite(t, gw)))
	c := gw.EC2Client(t)

	out, err := c.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(8),
		AvailabilityZone: aws.String(testAZ),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeVolume),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("data")}},
		}},
	})
	require.NoError(t, err, "CreateVolume")
	volumeID := aws.StringValue(out.VolumeId)
	require.NotEmpty(t, volumeID)

	desc, err := c.DescribeVolumes(&ec2.DescribeVolumesInput{VolumeIds: []*string{out.VolumeId}})
	require.NoError(t, err, "DescribeVolumes")
	require.Len(t, desc.Volumes, 1)
	require.Len(t, desc.Volumes[0].Tags, 1, "DescribeVolumes returns the creation tag")
	assert.Equal(t, "Name", aws.StringValue(desc.Volumes[0].Tags[0].Key))
	assert.Equal(t, "data", aws.StringValue(desc.Volumes[0].Tags[0].Value))

	tags, err := c.DescribeTags(&ec2.DescribeTagsInput{Filters: []*ec2.Filter{{
		Name: aws.String("resource-id"), Values: []*string{aws.String(volumeID)},
	}}})
	require.NoError(t, err, "DescribeTags")
	got := map[string]string{}
	for _, tag := range tags.Tags {
		assert.Equal(t, "volume", aws.StringValue(tag.ResourceType))
		got[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, map[string]string{"Name": "data"}, got, "DescribeTags must agree with DescribeVolumes")
}
