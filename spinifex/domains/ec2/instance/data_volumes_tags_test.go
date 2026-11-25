package instance_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An extra RunInstances disk is created by the real volume service, so its
// record carries the request's volume tags exactly as a CreateVolume does.
func TestPrepareDataVolumes_ExtraDiskRecordCarriesVolumeTags(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{AZ: ec2instance.TestRootAZ, Predastore: config.PredastoreConfig{Bucket: ec2instance.TestRootBucket}}
	volumes := ec2volume.NewVolumeServiceImplWithStore(cfg, store, nil)
	volumes.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	svc := ec2instance.NewProviderRootVolumeTestServiceWithStore(t, store)
	svc.SetVolumeCreator(volumes)

	input := dataMappingInput("/dev/sdf")
	input.TagSpecifications = []*ec2.TagSpecification{
		{ResourceType: aws.String(ec2.ResourceTypeInstance), Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("node")}}},
		{ResourceType: aws.String(ec2.ResourceTypeVolume), Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("extra-disk")}}},
	}
	infos, err := svc.PrepareDataVolumes(context.Background(), input, &vm.VM{AccountID: ec2instance.TestRootAccount})
	require.NoError(t, err)
	require.Len(t, infos, 1)

	described, err := volumes.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(infos[0].VolumeId)},
	}, ec2instance.TestRootAccount)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	got := map[string]string{}
	for _, tag := range described.Volumes[0].Tags {
		got[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, map[string]string{"Name": "extra-disk"}, got)
}
