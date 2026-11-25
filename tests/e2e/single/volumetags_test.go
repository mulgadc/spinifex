//go:build e2e

package single

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runRootVolumeTagsInDescribeTags launches an instance with volume
// TagSpecifications and checks that DescribeTags returns them for the root
// volume, as DescribeVolumes does. Terraform reads volume_tags this way.
func runRootVolumeTagsInDescribeTags(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — RunInstances volume tags reach DescribeTags")

	instType, _ := harness.DiscoverNanoInstanceType(t, fix.Harness)
	amiID := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	tagValue := fmt.Sprintf("e2e-voltags-%d", time.Now().UnixNano())

	harness.Step(t, "run-instances with volume tag e2e-voltags=%s", tagValue)
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: []*string{aws.String(vpc.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeVolume),
			Tags:         []*ec2.Tag{{Key: aws.String("e2e-voltags"), Value: aws.String(tagValue)}},
		}},
	})
	require.NoError(t, err, "run-instances")
	require.NotEmpty(t, out.Instances, "run-instances returned no instances")
	instanceID := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
		harness.WaitForInstanceState(t, fix.AWS, instanceID, "terminated")
	})
	harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")

	inst := describeSingletonInstance(t, fix, instanceID)
	var rootVolumeID string
	for _, bdm := range inst.BlockDeviceMappings {
		if aws.StringValue(bdm.DeviceName) == aws.StringValue(inst.RootDeviceName) && bdm.Ebs != nil {
			rootVolumeID = aws.StringValue(bdm.Ebs.VolumeId)
		}
	}
	require.NotEmpty(t, rootVolumeID, "no root volume in BlockDeviceMappings")
	harness.Detail(t, "instance_id", instanceID, "root_volume_id", rootVolumeID)

	harness.Step(t, "describe-volumes %s carries the tag", rootVolumeID)
	vols, err := fix.AWS.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(rootVolumeID)}})
	require.NoError(t, err, "describe-volumes")
	require.Len(t, vols.Volumes, 1)
	assert.Equal(t, tagValue, volumeTagValue(vols.Volumes[0], "e2e-voltags"), "DescribeVolumes returns the creation tag")

	harness.Step(t, "describe-tags resource-id=%s agrees", rootVolumeID)
	tags, err := fix.AWS.EC2.DescribeTags(&ec2.DescribeTagsInput{Filters: []*ec2.Filter{{
		Name: aws.String("resource-id"), Values: []*string{aws.String(rootVolumeID)},
	}}})
	require.NoError(t, err, "describe-tags")
	got := map[string]string{}
	for _, tag := range tags.Tags {
		assert.Equal(t, "volume", aws.StringValue(tag.ResourceType))
		got[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, map[string]string{"e2e-voltags": tagValue}, got, "DescribeTags must agree with DescribeVolumes")
}

func volumeTagValue(vol *ec2.Volume, key string) string {
	for _, tag := range vol.Tags {
		if aws.StringValue(tag.Key) == key {
			return aws.StringValue(tag.Value)
		}
	}
	return ""
}
