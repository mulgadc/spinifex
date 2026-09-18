package handlers_ec2_instance

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVolumeCreator stands in for the volume service, recording what a launch
// asked it to create and what it was asked to delete again.
type fakeVolumeCreator struct {
	created  []*ec2.CreateVolumeInput
	deleted  []string
	failFrom int
	metadata *ebsmetadata.Store
}

func (f *fakeVolumeCreator) CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	if f.failFrom > 0 && len(f.created) >= f.failFrom {
		return nil, errors.New("create refused")
	}
	f.created = append(f.created, input)
	id := "vol-data-" + string(rune('a'+len(f.created)-1))
	if f.metadata != nil {
		doc := ebsmetadata.Volume{
			VolumeID: id, TenantID: accountID,
			CapacityGiB: safecast.Int64ToUint64(aws.Int64Value(input.Size)),
			State:       string(ebsprovider.VolumeStateAvailable),
		}
		if err := f.metadata.PutVolume(ctx, doc); err != nil {
			return nil, err
		}
	}
	return &ec2.Volume{VolumeId: aws.String(id), Size: input.Size}, nil
}

func (f *fakeVolumeCreator) DeleteVolume(_ context.Context, input *ec2.DeleteVolumeInput, _ string) (*ec2.DeleteVolumeOutput, error) {
	f.deleted = append(f.deleted, aws.StringValue(input.VolumeId))
	return &ec2.DeleteVolumeOutput{}, nil
}

// dataVolumeService builds an instance service with a metadata store and the
// fake creator wired in, which is all the data-volume path touches.
func dataVolumeService(t *testing.T, creator *fakeVolumeCreator) *InstanceServiceImpl {
	t.Helper()
	svc := providerRootVolumeService(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}),
		rootVolumeAMILoader(), objectstore.NewMemoryObjectStore())
	creator.metadata = svc.metadata
	svc.volumeCreator = creator
	return svc
}

func dataMappingInput(devices ...string) *ec2.RunInstancesInput {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{DeviceName: aws.String(ebsmetadata.RootDeviceName), Ebs: &ec2.EbsBlockDevice{VolumeSize: aws.Int64(8)}},
		},
	}
	for i, device := range devices {
		input.BlockDeviceMappings = append(input.BlockDeviceMappings, &ec2.BlockDeviceMapping{
			DeviceName: aws.String(device),
			Ebs:        &ec2.EbsBlockDevice{VolumeSize: aws.Int64(int64(4 + i))},
		})
	}
	return input
}

// The mapping order the caller gave is the order the volumes are created in,
// and each is attached at the device name the request named.
func TestPrepareDataVolumes_CreatesOnePerMapping(t *testing.T) {
	creator := &fakeVolumeCreator{}
	svc := dataVolumeService(t, creator)
	instance := &vm.VM{AccountID: testRootAccount}

	infos, err := svc.prepareDataVolumes(context.Background(), dataMappingInput("/dev/sdf", "/dev/sdg"), instance)
	require.NoError(t, err)
	require.Len(t, infos, 2)
	assert.Equal(t, "/dev/sdf", infos[0].DeviceName)
	assert.Equal(t, "/dev/sdg", infos[1].DeviceName)

	require.Len(t, creator.created, 2)
	assert.Equal(t, int64(4), aws.Int64Value(creator.created[0].Size))
	assert.Equal(t, int64(5), aws.Int64Value(creator.created[1].Size))
	assert.Equal(t, testRootAZ, aws.StringValue(creator.created[0].AvailabilityZone))

	require.Len(t, instance.EBSRequests.Requests, 2)
	for i, req := range instance.EBSRequests.Requests {
		assert.False(t, req.Boot, "a data volume must not be marked bootable")
		assert.Equal(t, infos[i].VolumeId, req.Name)
		assert.Equal(t, infos[i].DeviceName, req.DeviceName)
		assert.True(t, req.DeleteOnTermination, "a launch-created volume defaults to delete on termination")
	}
}

func TestPrepareDataVolumes_NoMappingsCreatesNothing(t *testing.T) {
	creator := &fakeVolumeCreator{}
	svc := dataVolumeService(t, creator)
	instance := &vm.VM{AccountID: testRootAccount}

	infos, err := svc.prepareDataVolumes(context.Background(), dataMappingInput(), instance)
	require.NoError(t, err)
	assert.Empty(t, infos)
	assert.Empty(t, creator.created)
	assert.Empty(t, instance.EBSRequests.Requests)
}

// A launch that fails partway has already created volumes nothing will ever
// attach, so it deletes them rather than leaving them behind.
func TestPrepareDataVolumes_UnwindsWhatItCreated(t *testing.T) {
	creator := &fakeVolumeCreator{failFrom: 1}
	svc := dataVolumeService(t, creator)
	instance := &vm.VM{AccountID: testRootAccount}

	_, err := svc.prepareDataVolumes(context.Background(), dataMappingInput("/dev/sdf", "/dev/sdg"), instance)
	require.Error(t, err)
	assert.Len(t, creator.created, 1)
	assert.Equal(t, []string{"vol-data-a"}, creator.deleted)
}

// DeleteOnTermination is what the terminate sweep reads, and the metadata copy
// is what DescribeVolumes reports as the attachment's flag.
func TestPrepareDataVolumes_RecordsDeleteOnTermination(t *testing.T) {
	creator := &fakeVolumeCreator{}
	svc := dataVolumeService(t, creator)
	instance := &vm.VM{AccountID: testRootAccount}

	input := dataMappingInput("/dev/sdf")
	input.BlockDeviceMappings[1].Ebs.DeleteOnTermination = aws.Bool(false)
	infos, err := svc.prepareDataVolumes(context.Background(), input, instance)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.False(t, infos[0].DeleteOnTermination)
	require.Len(t, instance.EBSRequests.Requests, 1)
	assert.False(t, instance.EBSRequests.Requests[0].DeleteOnTermination)

	doc, err := svc.metadata.GetVolume(context.Background(), testRootAccount, infos[0].VolumeId)
	require.NoError(t, err)
	assert.False(t, doc.DeleteOnTermination)

	kept := &fakeVolumeCreator{}
	keptSvc := dataVolumeService(t, kept)
	keptInfos, err := keptSvc.prepareDataVolumes(context.Background(), dataMappingInput("/dev/sdf"), &vm.VM{AccountID: testRootAccount})
	require.NoError(t, err)
	require.Len(t, keptInfos, 1)
	doc, err = keptSvc.metadata.GetVolume(context.Background(), testRootAccount, keptInfos[0].VolumeId)
	require.NoError(t, err)
	assert.True(t, doc.DeleteOnTermination)
}

// parseDataVolumeParams decides which mappings are data volumes at all.
func TestParseDataVolumeParams_SkipsRootAndEphemeral(t *testing.T) {
	input := &ec2.RunInstancesInput{
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{DeviceName: aws.String("/dev/sdf"), Ebs: &ec2.EbsBlockDevice{VolumeSize: aws.Int64(4)}},
			{DeviceName: aws.String(ebsmetadata.RootDeviceName), Ebs: &ec2.EbsBlockDevice{VolumeSize: aws.Int64(8)}},
			{DeviceName: aws.String("/dev/sdb"), VirtualName: aws.String("ephemeral0")},
		},
	}
	params := parseDataVolumeParams(input)
	require.Len(t, params, 1)
	assert.Equal(t, "/dev/sdf", params[0].deviceName)
	assert.Equal(t, int64(4), params[0].sizeGiB)
	assert.True(t, params[0].deleteOnTermination)
}
