package handlers_ec2_instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/tags"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mgrWith returns a vm.Manager pre-populated with vms. Test fixture for
// services that previously took a *vm.Instances; post-vm.Manager refactor we
// build a Manager and Replace its set rather than inlining a map.
func mgrWith(vms map[string]*vm.VM) *vm.Manager {
	m := vm.NewManager()
	if len(vms) > 0 {
		m.Replace(vms)
	}
	return m
}

func TestRunInstance_Success(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
		"t3.small": {InstanceType: aws.String("t3.small")},
	}

	svc := &InstanceServiceImpl{
		instanceTypes: instanceTypes,
	}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("my-key"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)

	require.NoError(t, err)
	require.NotNil(t, instance)
	require.NotNil(t, ec2Instance)

	// Verify VM struct
	assert.Contains(t, instance.ID, "i-")
	assert.Equal(t, vm.StateProvisioning, instance.Status)
	assert.Equal(t, "t3.micro", instance.InstanceType)
	assert.Equal(t, input, instance.RunInstancesInput)
	assert.Equal(t, ec2Instance, instance.Instance)

	// Verify EC2 metadata
	assert.Equal(t, instance.ID, *ec2Instance.InstanceId)
	assert.Equal(t, "t3.micro", *ec2Instance.InstanceType)
	assert.Equal(t, "ami-0abcdef1234567890", *ec2Instance.ImageId)
	assert.Equal(t, "my-key", *ec2Instance.KeyName)
	assert.Equal(t, int64(0), *ec2Instance.State.Code)
	assert.Equal(t, "pending", *ec2Instance.State.Name)
	assert.NotNil(t, ec2Instance.LaunchTime)
}

// Architecture is projected onto the customer instance from the instance type at
// launch, so it flows to DescribeInstances and the IMDS identity document. Guards
// the platform-wide describe-instances change, not just IMDS.
func TestRunInstance_ArchitecturePopulated(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {
			InstanceType:  aws.String("t3.micro"),
			ProcessorInfo: &ec2.ProcessorInfo{SupportedArchitectures: []*string{aws.String("x86_64")}},
		},
		"t4g.micro": {
			InstanceType:  aws.String("t4g.micro"),
			ProcessorInfo: &ec2.ProcessorInfo{SupportedArchitectures: []*string{aws.String("arm64")}},
		},
	}
	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	for typ, wantArch := range map[string]string{"t3.micro": "x86_64", "t4g.micro": "arm64"} {
		_, ec2Instance, err := svc.RunInstance(&ec2.RunInstancesInput{
			ImageId:      aws.String("ami-0abcdef1234567890"),
			InstanceType: aws.String(typ),
		})
		require.NoError(t, err)
		require.NotNil(t, ec2Instance.Architecture, "type=%s", typ)
		assert.Equal(t, wantArch, *ec2Instance.Architecture, "type=%s", typ)
	}
}

func TestRunInstance_WithIamInstanceProfile(t *testing.T) {
	const profileARN = "arn:aws:iam::111122223333:instance-profile/app-profile"
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}
	input := &ec2.RunInstancesInput{
		ImageId:            aws.String("ami-012345"),
		InstanceType:       aws.String("t3.micro"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Arn: aws.String(profileARN)},
	}

	instance, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)

	assert.Equal(t, profileARN, instance.IamInstanceProfileArn,
		"vm.VM must record the canonical ARN supplied by the gateway")
	assert.True(t, strings.HasPrefix(instance.IamInstanceProfileAssociationId, "iip-assoc-"),
		"daemon must generate an AWS-style association ID at launch")
	require.NotNil(t, ec2Instance.IamInstanceProfile)
	assert.Equal(t, profileARN, aws.StringValue(ec2Instance.IamInstanceProfile.Arn))
	// Id is deliberately left empty here — daemons have no IAM access, the
	// gateway enriches Id from the resolved profile.
	assert.Nil(t, ec2Instance.IamInstanceProfile.Id,
		"daemon must not emit InstanceProfileID — that is the gateway's responsibility")
}

func TestRunInstance_AppliesInstanceTags(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String("spinifex:eks-cluster"), Value: aws.String("eks-quickstart")},
					{Key: aws.String("spinifex:eks-nodegroup"), Value: aws.String("default")},
				},
			},
			// A non-instance spec must not leak onto the instance.
			{
				ResourceType: aws.String("volume"),
				Tags:         []*ec2.Tag{{Key: aws.String("ignored"), Value: aws.String("vol")}},
			},
		},
	}

	_, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)

	// Tags must surface on the instance metadata so DescribeInstances returns
	// them and the node group can discover its workers by tag.
	got := map[string]string{}
	for _, tg := range ec2Instance.Tags {
		got[aws.StringValue(tg.Key)] = aws.StringValue(tg.Value)
	}
	assert.Equal(t, "eks-quickstart", got["spinifex:eks-cluster"])
	assert.Equal(t, "default", got["spinifex:eks-nodegroup"])
	assert.NotContains(t, got, "ignored", "volume-scoped tags must not land on the instance")
}

func TestRunInstance_NoTagSpecifications(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
	}

	_, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)
	assert.Empty(t, ec2Instance.Tags)
}

func TestRunInstance_IamInstanceProfileEmptyARNIgnored(t *testing.T) {
	// AWS SDKs sometimes round-trip an IamInstanceProfile with both fields
	// empty. Treat that as "no profile attached" rather than persisting a
	// half-baked binding.
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}
	input := &ec2.RunInstancesInput{
		ImageId:            aws.String("ami-012345"),
		InstanceType:       aws.String("t3.micro"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Arn: aws.String("")},
	}
	instance, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)
	assert.Empty(t, instance.IamInstanceProfileArn)
	assert.Empty(t, instance.IamInstanceProfileAssociationId)
	assert.Nil(t, ec2Instance.IamInstanceProfile)
}

func TestRunInstance_NoIamInstanceProfile(t *testing.T) {
	svc := &InstanceServiceImpl{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{"t3.micro": {InstanceType: aws.String("t3.micro")}},
	}
	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
	}
	instance, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)
	assert.Empty(t, instance.IamInstanceProfileArn)
	assert.Empty(t, instance.IamInstanceProfileAssociationId)
	assert.Nil(t, ec2Instance.IamInstanceProfile)
}

func TestRunInstance_NoKeyName(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)

	require.NoError(t, err)
	require.NotNil(t, instance)
	assert.Nil(t, ec2Instance.KeyName)
}

func TestRunInstance_InvalidInstanceType(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("nonexistent.type"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceType, err.Error())
	assert.Nil(t, instance)
	assert.Nil(t, ec2Instance)
}

func TestRunInstance_UniqueIDs(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
	}

	instance1, _, err1 := svc.RunInstance(input)
	instance2, _, err2 := svc.RunInstance(input)

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.NotEqual(t, instance1.ID, instance2.ID, "Each instance should have a unique ID")
}

func TestFloorVolumeSizeToAMI(t *testing.T) {
	loader := &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
		"ami-rocky":   {VolumeSizeGiB: 10},
		"ami-debian":  {VolumeSizeGiB: 3},
		"ami-no-size": {VolumeSizeGiB: 0},
	}}
	const fourGiB = 4 * 1024 * 1024 * 1024
	const tenGiB = 10 * 1024 * 1024 * 1024
	const twentyGiB = 20 * 1024 * 1024 * 1024

	t.Run("ami larger than requested rounds up", func(t *testing.T) {
		assert.Equal(t, tenGiB, floorVolumeSizeToAMI(context.Background(), loader, "ami-rocky", fourGiB))
	})
	t.Run("ami smaller than requested keeps requested", func(t *testing.T) {
		assert.Equal(t, fourGiB, floorVolumeSizeToAMI(context.Background(), loader, "ami-debian", fourGiB))
	})
	t.Run("requested larger than ami keeps requested", func(t *testing.T) {
		assert.Equal(t, twentyGiB, floorVolumeSizeToAMI(context.Background(), loader, "ami-rocky", twentyGiB))
	})
	t.Run("missing VolumeSizeGiB keeps requested (legacy AMI)", func(t *testing.T) {
		assert.Equal(t, fourGiB, floorVolumeSizeToAMI(context.Background(), loader, "ami-no-size", fourGiB))
	})
	t.Run("unknown AMI keeps requested", func(t *testing.T) {
		assert.Equal(t, fourGiB, floorVolumeSizeToAMI(context.Background(), loader, "ami-unknown", fourGiB))
	})
	t.Run("non-ami image id keeps requested", func(t *testing.T) {
		assert.Equal(t, fourGiB, floorVolumeSizeToAMI(context.Background(), loader, "vol-123", fourGiB))
	})
	t.Run("nil loader keeps requested", func(t *testing.T) {
		assert.Equal(t, fourGiB, floorVolumeSizeToAMI(context.Background(), nil, "ami-rocky", fourGiB))
	})
}

func TestRunInstance_NoImageId(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)
	require.NotNil(t, instance)
	assert.Nil(t, ec2Instance.ImageId)
	assert.Nil(t, ec2Instance.KeyName)
}

func TestParseVolumeParams_Defaults(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-0abcdef1234567890"),
	}

	p := parseVolumeParams(input)

	assert.Equal(t, 4*1024*1024*1024, p.size, "default size should be 4GB")
	assert.Equal(t, "/dev/vda", p.deviceName, "default device should be /dev/vda")
	assert.True(t, p.deleteOnTermination, "deleteOnTermination should default to true")
	assert.Empty(t, p.volumeType)
	assert.Zero(t, p.iops)
	// AMI-based: imageId should be a generated vol-*, snapshotId should be the AMI
	assert.True(t, strings.HasPrefix(p.imageId, "vol-"), "AMI launch should generate vol- ID")
	assert.Equal(t, "ami-0abcdef1234567890", p.snapshotId)
}

func TestParseVolumeParams_CustomBlockDeviceMapping(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-test123"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize:          aws.Int64(20),
					VolumeType:          aws.String("gp3"),
					Iops:                aws.Int64(3000),
					DeleteOnTermination: aws.Bool(false),
				},
			},
		},
	}

	p := parseVolumeParams(input)

	assert.Equal(t, 20*1024*1024*1024, p.size, "size should be 20 GiB in bytes")
	assert.Equal(t, "/dev/sda1", p.deviceName)
	assert.Equal(t, "gp3", p.volumeType)
	assert.Equal(t, 3000, p.iops)
	assert.False(t, p.deleteOnTermination)
}

func TestParseVolumeParams_BlockDeviceMappingNoEbs(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-test"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
			},
		},
	}

	p := parseVolumeParams(input)

	assert.Equal(t, "/dev/xvda", p.deviceName)
	assert.Equal(t, 4*1024*1024*1024, p.size, "size should stay at default without Ebs")
	assert.True(t, p.deleteOnTermination)
}

func TestParseVolumeParams_NonAMIImageId(t *testing.T) {
	rawImageId := "vol-existing-volume-id"
	input := &ec2.RunInstancesInput{
		ImageId: aws.String(rawImageId),
	}

	p := parseVolumeParams(input)

	assert.Equal(t, rawImageId, p.imageId, "non-AMI imageId should be used directly")
	assert.Empty(t, p.snapshotId, "non-AMI launch should have no snapshotId")
}

func TestParseVolumeParams_PartialEbs(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-partial"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(8),
				},
			},
		},
	}

	p := parseVolumeParams(input)

	assert.Equal(t, 8*1024*1024*1024, p.size)
	assert.Equal(t, "/dev/vda", p.deviceName, "device should stay at default")
	assert.Empty(t, p.volumeType, "volumeType should stay empty")
	assert.Zero(t, p.iops, "iops should stay zero")
	assert.True(t, p.deleteOnTermination, "deleteOnTermination should stay at default")
}

func TestRunInstance_WithTags(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}
	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("test-vm")},
					{Key: aws.String("env"), Value: aws.String("dev")},
				},
			},
		},
	}

	instance, _, err := svc.RunInstance(input)
	require.NoError(t, err)
	// Tags are stored in RunInstancesInput which is preserved on the VM
	assert.Equal(t, input, instance.RunInstancesInput)
	assert.Len(t, input.TagSpecifications, 1)
	assert.Len(t, input.TagSpecifications[0].Tags, 2)
}

func TestRunInstance_WithPlacement(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}
	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
		Placement: &ec2.Placement{
			GroupName: aws.String("my-pg"),
		},
	}

	instance, _, err := svc.RunInstance(input)
	require.NoError(t, err)
	assert.Equal(t, "my-pg", *instance.RunInstancesInput.Placement.GroupName)
}

func TestParseVolumeParams_MultipleBlockDeviceMappings(t *testing.T) {
	// parseVolumeParams only uses the first block device mapping
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-multi"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(30),
					VolumeType: aws.String("gp3"),
				},
			},
			{
				DeviceName: aws.String("/dev/sdb"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(100),
					VolumeType: aws.String("io1"),
					Iops:       aws.Int64(5000),
				},
			},
		},
	}

	p := parseVolumeParams(input)
	// First mapping wins for root volume
	assert.Equal(t, 30*1024*1024*1024, p.size)
	assert.Equal(t, "/dev/sda1", p.deviceName)
	assert.Equal(t, "gp3", p.volumeType)
}

func TestParseVolumeParams_Io1WithIops(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-io1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(100),
					VolumeType: aws.String("io1"),
					Iops:       aws.Int64(5000),
				},
			},
		},
	}

	p := parseVolumeParams(input)
	assert.Equal(t, 100*1024*1024*1024, p.size)
	assert.Equal(t, "io1", p.volumeType)
	assert.Equal(t, 5000, p.iops)
}

// --- Describe* coverage -----------------------------------------------------

type fakeResourceCapacityProvider struct {
	types          []*ec2.InstanceTypeInfo
	supportedTypes []*ec2.InstanceTypeInfo
	gotShowCap     bool
	calls          int
	supportedCalls int
	instanceTypes  map[string]*ec2.InstanceTypeInfo
	allocateErr    error
	allocated      []*ec2.InstanceTypeInfo
	deallocated    []*ec2.InstanceTypeInfo
	canAllocFn     func(*ec2.InstanceTypeInfo, int) int

	reservationAllocErr  error
	reservationAllocated []*ec2.InstanceTypeInfo
	reservationReleased  []*ec2.InstanceTypeInfo
	reservationAvailFn   func(string, string, *ec2.InstanceTypeInfo) int

	memPressure bool
}

func (f *fakeResourceCapacityProvider) HostUnderMemoryPressure() bool { return f.memPressure }

func (f *fakeResourceCapacityProvider) GetAvailableInstanceTypeInfos(showCapacity bool) []*ec2.InstanceTypeInfo {
	f.calls++
	f.gotShowCap = showCapacity
	return f.types
}

func (f *fakeResourceCapacityProvider) GetSupportedInstanceTypeInfos() []*ec2.InstanceTypeInfo {
	f.supportedCalls++
	if f.supportedTypes != nil {
		return f.supportedTypes
	}
	return f.types
}

func (f *fakeResourceCapacityProvider) Allocate(it *ec2.InstanceTypeInfo) error {
	if f.allocateErr != nil {
		return f.allocateErr
	}
	f.allocated = append(f.allocated, it)
	return nil
}

func (f *fakeResourceCapacityProvider) Deallocate(it *ec2.InstanceTypeInfo) {
	f.deallocated = append(f.deallocated, it)
}

func (f *fakeResourceCapacityProvider) CanAllocate(it *ec2.InstanceTypeInfo, count int) int {
	if f.canAllocFn != nil {
		return f.canAllocFn(it, count)
	}
	return count
}

func (f *fakeResourceCapacityProvider) AllocateFromReservation(_, _ string, it *ec2.InstanceTypeInfo) error {
	if f.reservationAllocErr != nil {
		return f.reservationAllocErr
	}
	f.reservationAllocated = append(f.reservationAllocated, it)
	return nil
}

func (f *fakeResourceCapacityProvider) ReleaseToReservation(_ string, it *ec2.InstanceTypeInfo) {
	f.reservationReleased = append(f.reservationReleased, it)
}

func (f *fakeResourceCapacityProvider) ReservationAvailable(reservationID, accountID string, it *ec2.InstanceTypeInfo) int {
	if f.reservationAvailFn != nil {
		return f.reservationAvailFn(reservationID, accountID, it)
	}
	return 0
}

func (f *fakeResourceCapacityProvider) InstanceTypes() map[string]*ec2.InstanceTypeInfo {
	return f.instanceTypes
}

type fakeStoppedStore struct {
	stopped         []*vm.VM
	terminated      []*vm.VM
	loadByID        map[string]*vm.VM
	wroteStopped    map[string]*vm.VM
	wroteTerminated map[string]*vm.VM
	deletedStopped  []string
	listErr         error
	listTermErr     error
	loadErr         error
	writeErr        error
	writeTermErr    error
	deleteErr       error
	deleteFailFirst bool
	deleteAttempts  int
}

func (f *fakeStoppedStore) LoadStoppedInstance(id string) (*vm.VM, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if v, ok := f.loadByID[id]; ok {
		return v, nil
	}
	return nil, nil
}
func (f *fakeStoppedStore) ListStoppedInstances() ([]*vm.VM, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stopped, nil
}
func (f *fakeStoppedStore) ListTerminatedInstances() ([]*vm.VM, error) {
	if f.listTermErr != nil {
		return nil, f.listTermErr
	}
	return f.terminated, nil
}
func (f *fakeStoppedStore) WriteStoppedInstance(id string, instance *vm.VM) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.wroteStopped == nil {
		f.wroteStopped = make(map[string]*vm.VM)
	}
	f.wroteStopped[id] = instance
	return nil
}
func (f *fakeStoppedStore) WriteTerminatedInstance(id string, instance *vm.VM) error {
	if f.writeTermErr != nil {
		return f.writeTermErr
	}
	if f.wroteTerminated == nil {
		f.wroteTerminated = make(map[string]*vm.VM)
	}
	f.wroteTerminated[id] = instance
	return nil
}
func (f *fakeStoppedStore) DeleteStoppedInstance(id string) error {
	f.deleteAttempts++
	if f.deleteFailFirst && f.deleteAttempts == 1 {
		return errors.New("transient delete failure")
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedStopped = append(f.deletedStopped, id)
	return nil
}
func (f *fakeStoppedStore) DeleteTerminatedInstance(string) error { return nil }

func TestDescribeInstanceTypes_NilResourceMgr(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeInstanceTypes_ReturnsSupportedByDefault(t *testing.T) {
	prov := &fakeResourceCapacityProvider{
		supportedTypes: []*ec2.InstanceTypeInfo{
			{InstanceType: aws.String("t3.micro")},
			{InstanceType: aws.String("t3.small")},
			{InstanceType: aws.String("m5.large")},
		},
		types: []*ec2.InstanceTypeInfo{
			{InstanceType: aws.String("t3.micro")},
		},
	}
	svc := &InstanceServiceImpl{resourceMgr: prov}

	out, err := svc.DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{}, "")
	require.NoError(t, err)
	require.Len(t, out.InstanceTypes, 3,
		"no-filter DescribeInstanceTypes must return the supported set, not the capacity-gated set")
	assert.Equal(t, 1, prov.supportedCalls, "supported-types path must be hit when capacity filter absent")
	assert.Equal(t, 0, prov.calls, "capacity-gated path must not be hit when capacity filter absent")
}

func TestDescribeInstanceTypes_CapacityFilterHitsAvailable(t *testing.T) {
	prov := &fakeResourceCapacityProvider{
		types: []*ec2.InstanceTypeInfo{
			{InstanceType: aws.String("t3.micro")},
			{InstanceType: aws.String("t3.micro")},
		},
		supportedTypes: []*ec2.InstanceTypeInfo{
			{InstanceType: aws.String("t3.micro")},
			{InstanceType: aws.String("m5.large")},
		},
	}
	svc := &InstanceServiceImpl{resourceMgr: prov}

	input := &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("capacity"), Values: []*string{aws.String("true")}},
		},
	}
	out, err := svc.DescribeInstanceTypes(context.Background(), input, "")
	require.NoError(t, err)
	require.Len(t, out.InstanceTypes, 2, "capacity=true must use the per-slot list")
	assert.Equal(t, 1, prov.calls, "capacity-gated path must be hit when capacity=true")
	assert.True(t, prov.gotShowCap, "capacity=true filter must reach the provider")
	assert.Equal(t, 0, prov.supportedCalls, "supported-types path must not be hit when capacity=true")
}

func TestDescribeInstances_Empty(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Reservations)
}

func TestDescribeInstances_OneVisibleInstance(t *testing.T) {
	id := "i-aaa111"
	resID := "r-1"
	owner := "111122223333"
	v := &vm.VM{
		ID:           id,
		InstanceType: "t3.micro",
		Status:       vm.StateRunning,
		AccountID:    owner,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String(resID),
			OwnerId:       aws.String(owner),
		},
		Instance: &ec2.Instance{InstanceId: aws.String(id), InstanceType: aws.String("t3.micro")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, resID, *out.Reservations[0].ReservationId)
	require.Len(t, out.Reservations[0].Instances, 1)
	assert.Equal(t, id, *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeInstances_AccountFilteringHidesOtherTenant(t *testing.T) {
	v := &vm.VM{
		ID:        "i-other",
		AccountID: "999988887777",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-other"),
			OwnerId:       aws.String("999988887777"),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-other")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, "111122223333")
	require.NoError(t, err)
	assert.Empty(t, out.Reservations)
}

// EKS control-plane VMs are owned by the customer account (their ENI lives in
// the customer VPC) but are platform-managed system instances, so they must be
// hidden from the owning customer's DescribeInstances the same way LB VMs are.
func TestDescribeInstances_HidesManagedSystemVMFromCustomer(t *testing.T) {
	owner := "111122223333"
	v := &vm.VM{
		ID:        "i-ekscp",
		AccountID: owner,
		ManagedBy: tags.ManagedByEKS,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-ekscp"),
			OwnerId:       aws.String(owner),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-ekscp")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	assert.Empty(t, out.Reservations, "managed system VM must not appear in customer listing")
}

// Root/operator callers still see managed system VMs.
func TestDescribeInstances_RootSeesManagedSystemVM(t *testing.T) {
	v := &vm.VM{
		ID:        "i-lb",
		AccountID: utils.GlobalAccountID,
		ManagedBy: tags.ManagedByELBv2,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-lb"),
			OwnerId:       aws.String(utils.GlobalAccountID),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-lb")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, utils.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-lb", *out.Reservations[0].Instances[0].InstanceId)
}

// A customer's own unmanaged instance (no ManagedBy) stays visible — confirms
// the exclusion keys on ManagedBy, not on account scope. Future EKS worker
// nodes (customer-owned, no ManagedBy tag) follow this path.
func TestDescribeInstances_CustomerWorkloadStaysVisible(t *testing.T) {
	owner := "111122223333"
	v := &vm.VM{
		ID:        "i-worker",
		AccountID: owner,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-worker"),
			OwnerId:       aws.String(owner),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-worker")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-worker", *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeInstances_MalformedID(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String("not-an-id")},
	}
	_, err := svc.DescribeInstances(context.Background(), input, utils.GlobalAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestDescribeInstances_FilterByInstanceID(t *testing.T) {
	keep := &vm.VM{
		ID: "i-keep",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-keep"),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-keep")},
	}
	drop := &vm.VM{
		ID: "i-drop",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-drop"),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-drop")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{
		keep.ID: keep,
		drop.ID: drop,
	})}

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-keep")}}
	out, err := svc.DescribeInstances(context.Background(), input, utils.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-keep", *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeInstanceAttribute_MissingInstanceID(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
		Attribute: aws.String(ec2.InstanceAttributeNameInstanceType),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribeInstanceAttribute_MissingAttribute(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-aaa"),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribeInstanceAttribute_RunningInstance(t *testing.T) {
	id := "i-attr1"
	owner := utils.GlobalAccountID
	v := &vm.VM{
		ID:           id,
		InstanceType: "t3.large",
		AccountID:    owner,
		RunInstancesInput: &ec2.RunInstancesInput{
			// base64("raw-user-data") — DescribeInstanceAttribute returns user-data base64-encoded.
			UserData: aws.String("cmF3LXVzZXItZGF0YQ=="),
		},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	tests := []struct {
		name       string
		attribute  string
		assertions func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput)
	}{
		{
			name:      "instanceType",
			attribute: ec2.InstanceAttributeNameInstanceType,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.InstanceType)
				assert.Equal(t, "t3.large", *out.InstanceType.Value)
			},
		},
		{
			name:      "userData",
			attribute: ec2.InstanceAttributeNameUserData,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.UserData)
				assert.Equal(t, "cmF3LXVzZXItZGF0YQ==", *out.UserData.Value)
			},
		},
		{
			name:      "disableApiTermination",
			attribute: ec2.InstanceAttributeNameDisableApiTermination,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.DisableApiTermination)
				assert.False(t, *out.DisableApiTermination.Value)
			},
		},
		{
			name:      "ebsOptimized",
			attribute: ec2.InstanceAttributeNameEbsOptimized,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.EbsOptimized)
				assert.False(t, *out.EbsOptimized.Value)
			},
		},
		{
			name:      "enaSupport",
			attribute: ec2.InstanceAttributeNameEnaSupport,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.EnaSupport)
				assert.True(t, *out.EnaSupport.Value)
			},
		},
		{
			name:      "sourceDestCheck",
			attribute: ec2.InstanceAttributeNameSourceDestCheck,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.SourceDestCheck)
				assert.True(t, *out.SourceDestCheck.Value)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
				InstanceId: aws.String(id),
				Attribute:  aws.String(tc.attribute),
			}, owner)
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, id, *out.InstanceId)
			tc.assertions(t, out)
		})
	}
}

func TestDescribeInstanceAttribute_NotRunning_NoStore(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-missing"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeInstanceAttribute_FoundInStoppedStore(t *testing.T) {
	id := "i-stopped1"
	owner := utils.GlobalAccountID
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, InstanceType: "t3.medium", AccountID: owner},
	}}
	svc := &InstanceServiceImpl{
		vmMgr:        mgrWith(map[string]*vm.VM{}),
		stoppedStore: store,
	}

	out, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(id),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, owner)
	require.NoError(t, err)
	assert.Equal(t, "t3.medium", *out.InstanceType.Value)
}

func TestDescribeInstanceAttribute_NotFound(t *testing.T) {
	svc := &InstanceServiceImpl{
		vmMgr:        mgrWith(map[string]*vm.VM{}),
		stoppedStore: &fakeStoppedStore{loadByID: map[string]*vm.VM{}},
	}
	_, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-ghost"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, utils.GlobalAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestDescribeInstanceAttribute_HiddenForOtherAccount(t *testing.T) {
	id := "i-other-acct"
	v := &vm.VM{ID: id, InstanceType: "t3.micro", AccountID: "999988887777"}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	_, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(id),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestDescribeInstanceAttribute_DisableApiTermination(t *testing.T) {
	owner := utils.GlobalAccountID

	tests := []struct {
		name     string
		instance *vm.VM
		want     bool
	}{
		{
			name: "flag true on RunInstancesInput",
			instance: &vm.VM{
				ID: "i-prot", AccountID: owner,
				RunInstancesInput: &ec2.RunInstancesInput{
					DisableApiTermination: aws.Bool(true),
				},
			},
			want: true,
		},
		{
			name: "flag false on RunInstancesInput",
			instance: &vm.VM{
				ID: "i-noprot", AccountID: owner,
				RunInstancesInput: &ec2.RunInstancesInput{
					DisableApiTermination: aws.Bool(false),
				},
			},
			want: false,
		},
		{
			name: "RunInstancesInput.DisableApiTermination nil",
			instance: &vm.VM{
				ID: "i-nilflag", AccountID: owner,
				RunInstancesInput: &ec2.RunInstancesInput{},
			},
			want: false,
		},
		{
			name:     "RunInstancesInput nil (legacy)",
			instance: &vm.VM{ID: "i-legacy", AccountID: owner},
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{tc.instance.ID: tc.instance})}
			out, err := svc.DescribeInstanceAttribute(context.Background(), &ec2.DescribeInstanceAttributeInput{
				InstanceId: aws.String(tc.instance.ID),
				Attribute:  aws.String(ec2.InstanceAttributeNameDisableApiTermination),
			}, owner)
			require.NoError(t, err)
			require.NotNil(t, out.DisableApiTermination)
			assert.Equal(t, tc.want, *out.DisableApiTermination.Value)
		})
	}
}

func TestDescribeStoppedInstances_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.DescribeStoppedInstances(context.Background(), &ec2.DescribeInstancesInput{}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeStoppedInstances_HappyPath(t *testing.T) {
	owner := utils.GlobalAccountID
	store := &fakeStoppedStore{
		stopped: []*vm.VM{
			{
				ID:        "i-stop1",
				AccountID: owner,
				Reservation: &ec2.Reservation{
					ReservationId: aws.String("r-stop1"),
					OwnerId:       aws.String(owner),
				},
				Instance: &ec2.Instance{InstanceId: aws.String("i-stop1")},
			},
		},
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	out, err := svc.DescribeStoppedInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-stop1", *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeTerminatedInstances_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.DescribeTerminatedInstances(context.Background(), &ec2.DescribeInstancesInput{}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeTerminatedInstances_HappyPath(t *testing.T) {
	owner := utils.GlobalAccountID
	store := &fakeStoppedStore{
		terminated: []*vm.VM{
			{
				ID:        "i-term1",
				AccountID: owner,
				Reservation: &ec2.Reservation{
					ReservationId: aws.String("r-term1"),
					OwnerId:       aws.String(owner),
				},
				Instance: &ec2.Instance{InstanceId: aws.String("i-term1")},
			},
		},
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	out, err := svc.DescribeTerminatedInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-term1", *out.Reservations[0].Instances[0].InstanceId)
}

func TestIsInstanceVisible(t *testing.T) {
	tests := []struct {
		name   string
		caller string
		owner  string
		want   bool
	}{
		{"empty owner, global caller", utils.GlobalAccountID, "", true},
		{"empty owner, non-global caller", "111122223333", "", false},
		{"matching account", "111122223333", "111122223333", true},
		{"different accounts", "111122223333", "999988887777", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsInstanceVisible(tc.caller, tc.owner))
		})
	}
}

func TestModifyInstanceAttribute_MissingInstanceID(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestModifyInstanceAttribute_SourceDestCheckTrueNoOp(t *testing.T) {
	// SourceDestCheck=true succeeds without touching KV or requiring stopped state.
	svc := &InstanceServiceImpl{}
	out, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-sdc-001"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, "acc")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestModifyInstanceAttribute_SourceDestCheckFalseUnsupported(t *testing.T) {
	// Disabling is unsupported: OVN port security always enforces the check.
	svc := &InstanceServiceImpl{}
	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String("i-sdc-001"),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnsupported, err.Error())
}

func TestModifyInstanceAttribute_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-1"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestModifyInstanceAttribute_InstanceNotFound(t *testing.T) {
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-missing"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestModifyInstanceAttribute_NotStopped(t *testing.T) {
	id := "i-running"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateRunning, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestModifyInstanceAttribute_NotVisible(t *testing.T) {
	id := "i-stopped"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "owner-acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "other-acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestModifyInstanceAttribute_ChangeInstanceType(t *testing.T) {
	id := "i-type"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {
			ID:           id,
			Status:       vm.StateStopped,
			AccountID:    "acc",
			InstanceType: "t3.micro",
			Config:       vm.Config{InstanceType: "t3.micro"},
			Instance: &ec2.Instance{
				InstanceId:   aws.String(id),
				InstanceType: aws.String("t3.micro"),
			},
		},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "acc")
	require.NoError(t, err)

	updated := store.wroteStopped[id]
	require.NotNil(t, updated)
	assert.Equal(t, "t3.medium", updated.InstanceType)
	assert.Equal(t, "t3.medium", updated.Config.InstanceType)
	assert.Equal(t, "t3.medium", *updated.Instance.InstanceType)
}

func TestModifyInstanceAttribute_ChangeInstanceType_EmptyValue(t *testing.T) {
	id := "i-empty"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc", Instance: &ec2.Instance{}},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("")},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, err.Error())
}

func TestModifyInstanceAttribute_ChangeInstanceType_NilEmbeddedInstance(t *testing.T) {
	id := "i-nil-inst"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestModifyInstanceAttribute_ChangeUserData(t *testing.T) {
	id := "i-ud"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {
			ID:        id,
			Status:    vm.StateStopped,
			AccountID: "acc",
			RunInstancesInput: &ec2.RunInstancesInput{
				UserData: aws.String("b2xk"),
			},
			Instance: &ec2.Instance{InstanceId: aws.String(id)},
		},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	newContent := "#!/bin/bash"
	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(id),
		UserData:   &ec2.BlobAttributeValue{Value: []byte(newContent)},
	}, "acc")
	require.NoError(t, err)

	updated := store.wroteStopped[id]
	require.NotNil(t, updated)
	assert.Equal(t, "IyEvYmluL2Jhc2g=", *updated.RunInstancesInput.UserData)
}

func TestModifyInstanceAttribute_ClearsStateReason(t *testing.T) {
	id := "i-recovery"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {
			ID:           id,
			Status:       vm.StateStopped,
			AccountID:    "acc",
			InstanceType: "m7i.small",
			Config:       vm.Config{InstanceType: "m7i.small"},
			Instance: &ec2.Instance{
				InstanceId:   aws.String(id),
				InstanceType: aws.String("m7i.small"),
				StateReason: &ec2.StateReason{
					Code:    aws.String("Server.InsufficientInstanceCapacity"),
					Message: aws.String("Instance type not available on any node"),
				},
			},
		},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.micro")},
	}, "acc")
	require.NoError(t, err)

	updated := store.wroteStopped[id]
	require.NotNil(t, updated)
	assert.Nil(t, updated.Instance.StateReason)
}

func TestModifyInstanceAttribute_DisableApiTermination_Running(t *testing.T) {
	const owner = "acc"
	tests := []struct {
		name     string
		initial  *ec2.RunInstancesInput
		setTo    bool
		wantFlag bool
	}{
		{name: "set true on empty input", initial: &ec2.RunInstancesInput{}, setTo: true, wantFlag: true},
		{name: "set true on nil input (legacy)", initial: nil, setTo: true, wantFlag: true},
		{name: "clear from true to false", initial: &ec2.RunInstancesInput{DisableApiTermination: aws.Bool(true)}, setTo: false, wantFlag: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := "i-run"
			mgr := mgrWith(map[string]*vm.VM{
				id: {ID: id, AccountID: owner, Status: vm.StateRunning, RunInstancesInput: tc.initial},
			})
			svc := &InstanceServiceImpl{vmMgr: mgr}

			_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
				InstanceId:            aws.String(id),
				DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(tc.setTo)},
			}, owner)
			require.NoError(t, err)

			got, _ := mgr.Get(id)
			require.NotNil(t, got.RunInstancesInput)
			require.NotNil(t, got.RunInstancesInput.DisableApiTermination)
			assert.Equal(t, tc.wantFlag, *got.RunInstancesInput.DisableApiTermination)
		})
	}
}

func TestModifyInstanceAttribute_DisableApiTermination_Running_NotVisible(t *testing.T) {
	id := "i-run-other"
	mgr := mgrWith(map[string]*vm.VM{
		id: {ID: id, AccountID: "owner-acc", Status: vm.StateRunning},
	})
	svc := &InstanceServiceImpl{vmMgr: mgr}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String(id),
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, "other-acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestModifyInstanceAttribute_DisableApiTermination_Stopped(t *testing.T) {
	id := "i-stop-prot"
	owner := "acc"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {
			ID: id, Status: vm.StateStopped, AccountID: owner,
			Instance: &ec2.Instance{InstanceId: aws.String(id)},
		},
	}}
	svc := &InstanceServiceImpl{
		vmMgr:        mgrWith(map[string]*vm.VM{}),
		stoppedStore: store,
	}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:            aws.String(id),
		DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	}, owner)
	require.NoError(t, err)

	updated := store.wroteStopped[id]
	require.NotNil(t, updated)
	require.NotNil(t, updated.RunInstancesInput)
	assert.True(t, *updated.RunInstancesInput.DisableApiTermination)
}

func TestModifyInstanceAttribute_WriteError(t *testing.T) {
	id := "i-werr"
	store := &fakeStoppedStore{
		loadByID: map[string]*vm.VM{
			id: {
				ID:           id,
				Status:       vm.StateStopped,
				AccountID:    "acc",
				InstanceType: "t3.micro",
				Config:       vm.Config{InstanceType: "t3.micro"},
				Instance: &ec2.Instance{
					InstanceId:   aws.String(id),
					InstanceType: aws.String("t3.micro"),
				},
			},
		},
		writeErr: fmt.Errorf("kv write boom"),
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.ModifyInstanceAttribute(context.Background(), &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(id),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.medium")},
	}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// --- TerminateStoppedInstance tests ---

type fakeVolumeDeleter struct {
	calls   []string
	deleted []string
	err     error
}

func (f *fakeVolumeDeleter) DeleteVolume(_ context.Context, input *ec2.DeleteVolumeInput, _ string) (*ec2.DeleteVolumeOutput, error) {
	id := aws.StringValue(input.VolumeId)
	f.calls = append(f.calls, id)
	if f.err != nil {
		return nil, f.err
	}
	f.deleted = append(f.deleted, id)
	return &ec2.DeleteVolumeOutput{}, nil
}

type fakeENIDeleter struct {
	calls []string
	err   error
}

func (f *fakeENIDeleter) DeleteNetworkInterface(_ context.Context, input *ec2.DeleteNetworkInterfaceInput, _ string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.calls = append(f.calls, aws.StringValue(input.NetworkInterfaceId))
	if f.err != nil {
		return nil, f.err
	}
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

type fakePublicIPReleaser struct {
	pool     string
	ip       string
	ownerENI string
	err      error
}

func (f *fakePublicIPReleaser) ReleaseIP(_ context.Context, pool, ip, ownerENIID string) error {
	f.pool = pool
	f.ip = ip
	f.ownerENI = ownerENIID
	return f.err
}

// embeddedNATS spins up an in-process NATS server scoped to the test and
// returns a connected client. Used for service tests that exercise the
// ebs.delete path inside TerminateStoppedInstance.
func embeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	return nc
}

func TestTerminateStoppedInstance_MissingInstanceID(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestTerminateStoppedInstance_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: "i-1"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestTerminateStoppedInstance_LoadError(t *testing.T) {
	store := &fakeStoppedStore{loadErr: errors.New("kv down")}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: "i-1"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestTerminateStoppedInstance_NotFound(t *testing.T) {
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: "i-missing"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestTerminateStoppedInstance_NotStopped(t *testing.T) {
	id := "i-running"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateRunning, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestTerminateStoppedInstance_NotVisible(t *testing.T) {
	id := "i-stopped"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "owner-acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "other-acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestTerminateStoppedInstance_TerminationProtected(t *testing.T) {
	id := "i-prot"
	v := &vm.VM{
		ID: id, Status: vm.StateStopped, AccountID: "acc",
		RunInstancesInput: &ec2.RunInstancesInput{
			DisableApiTermination: aws.Bool(true),
		},
	}
	v.EBSRequests.Requests = []spxtypes.EBSRequest{
		{Name: "vol-001", DeleteOnTermination: true},
	}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}
	vd := &fakeVolumeDeleter{}
	svc := &InstanceServiceImpl{stoppedStore: store, volumeDeleter: vd}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorOperationNotPermitted, err.Error())

	assert.Empty(t, vd.calls, "volumes must not be deleted when termination protected")
	assert.Empty(t, store.wroteTerminated, "must not write to terminated bucket when protected")
	assert.Empty(t, store.deletedStopped, "must not remove from stopped bucket when protected")
}

func TestTerminateStoppedInstance_HappyPath(t *testing.T) {
	id := "i-term-001"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	out, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "terminated", out.Status)
	assert.Equal(t, id, out.InstanceID)

	require.NotNil(t, store.wroteTerminated[id])
	assert.Equal(t, vm.StateTerminated, store.wroteTerminated[id].Status)
	assert.Contains(t, store.deletedStopped, id)
}

func TestTerminateStoppedInstance_CentralTagsDeleted(t *testing.T) {
	id := "i-term-tags"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc"},
	}}
	tw := &fakeTagWriter{}
	svc := &InstanceServiceImpl{stoppedStore: store, tagWriter: tw}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)

	assert.Equal(t, 1, tw.deleteCalls)
	assert.Equal(t, "acc", tw.accountID, "central delete must target the owner's namespace")
	assert.Equal(t, id, tw.resourceID)
}

func TestTerminateStoppedInstance_CentralTagDeleteFailureIsBestEffort(t *testing.T) {
	id := "i-term-tagerr"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc"},
	}}
	tw := &fakeTagWriter{err: errors.New("s3 down")}
	svc := &InstanceServiceImpl{stoppedStore: store, tagWriter: tw}

	out, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err, "terminate already succeeded; central delete failure must not surface")
	assert.Equal(t, "terminated", out.Status)
	assert.Contains(t, store.deletedStopped, id)
}

func TestTerminateStoppedInstance_ProtectedSkipsCentralTagDelete(t *testing.T) {
	id := "i-prot-tags"
	tw := &fakeTagWriter{}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {
			ID: id, Status: vm.StateStopped, AccountID: "acc",
			RunInstancesInput: &ec2.RunInstancesInput{DisableApiTermination: aws.Bool(true)},
		},
	}}
	svc := &InstanceServiceImpl{stoppedStore: store, tagWriter: tw}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Zero(t, tw.deleteCalls, "central tags must survive a rejected terminate")
}

func TestTerminateStoppedInstance_WriteTerminatedError_Aborts(t *testing.T) {
	id := "i-werr"
	store := &fakeStoppedStore{
		loadByID:     map[string]*vm.VM{id: {ID: id, Status: vm.StateStopped, AccountID: "acc"}},
		writeTermErr: errors.New("kv write boom"),
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, store.deletedStopped, "stopped delete must not run when terminated write fails")
}

func TestTerminateStoppedInstance_RetriesStoppedDelete(t *testing.T) {
	id := "i-retry"
	store := &fakeStoppedStore{
		loadByID:        map[string]*vm.VM{id: {ID: id, Status: vm.StateStopped, AccountID: "acc"}},
		deleteFailFirst: true,
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	assert.Equal(t, 2, store.deleteAttempts, "first delete fails, retry succeeds")
	assert.Contains(t, store.deletedStopped, id)
}

func TestTerminateStoppedInstance_UserVolumeDeleted(t *testing.T) {
	id := "i-vol"
	v := &vm.VM{ID: id, Status: vm.StateStopped, AccountID: "acc"}
	v.EBSRequests.Requests = []spxtypes.EBSRequest{
		{Name: "vol-user-001", DeleteOnTermination: true},
		{Name: "vol-keep-001", DeleteOnTermination: false},
	}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}
	vd := &fakeVolumeDeleter{}
	svc := &InstanceServiceImpl{stoppedStore: store, volumeDeleter: vd}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	assert.Equal(t, []string{"vol-user-001"}, vd.calls, "only DeleteOnTermination=true volumes deleted")
	assert.Equal(t, []string{"vol-user-001"}, vd.deleted)
}

func TestTerminateStoppedInstance_NoVolumeDeleterSkipsGracefully(t *testing.T) {
	id := "i-no-vd"
	v := &vm.VM{ID: id, Status: vm.StateStopped, AccountID: "acc"}
	v.EBSRequests.Requests = []spxtypes.EBSRequest{
		{Name: "vol-user-001", DeleteOnTermination: true},
	}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err, "missing VolumeDeleter must not abort termination")
	require.NotNil(t, store.wroteTerminated[id])
}

func TestTerminateStoppedInstance_InternalVolumesViaNATS(t *testing.T) {
	id := "i-int-vol"
	v := &vm.VM{ID: id, Status: vm.StateStopped, AccountID: "acc"}
	v.EBSRequests.Requests = []spxtypes.EBSRequest{
		{Name: "vol-efi-001", EFI: true},
	}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}

	nc := embeddedNATS(t)
	var ebsDeleted []string
	sub, err := nc.Subscribe("ebs.delete", func(msg *nats.Msg) {
		var req spxtypes.EBSDeleteRequest
		_ = json.Unmarshal(msg.Data, &req)
		ebsDeleted = append(ebsDeleted, req.Volume)
		_ = msg.Respond([]byte(`{"Success":true}`))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	svc := &InstanceServiceImpl{stoppedStore: store, natsConn: nc}

	_, err = svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"vol-efi-001"}, ebsDeleted)
}

func TestTerminateStoppedInstance_PublicIPReleased(t *testing.T) {
	id := "i-pubip"
	v := &vm.VM{
		ID:           id,
		Status:       vm.StateStopped,
		AccountID:    "acc",
		PublicIP:     "203.0.113.5",
		PublicIPPool: "pool-a",
		ENIId:        "eni-xyz",
		Instance: &ec2.Instance{
			InstanceId:       aws.String(id),
			VpcId:            aws.String("vpc-1"),
			PrivateIpAddress: aws.String("10.0.0.5"),
		},
	}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}
	pr := &fakePublicIPReleaser{}
	svc := &InstanceServiceImpl{stoppedStore: store, ipReleaser: pr}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	assert.Equal(t, "pool-a", pr.pool)
	assert.Equal(t, "203.0.113.5", pr.ip)
}

func TestTerminateStoppedInstance_ENIDeleted(t *testing.T) {
	id := "i-eni"
	v := &vm.VM{ID: id, Status: vm.StateStopped, AccountID: "acc", ENIId: "eni-1234"}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}
	ed := &fakeENIDeleter{}
	ec := &fakeENICreator{}
	svc := &InstanceServiceImpl{stoppedStore: store, eniDeleter: ed, eniCreator: ec}

	_, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	// Detach must precede the delete so a stale in-use attachment can't strand
	// the ENI record after the instance is gone.
	assert.Equal(t, 1, ec.detachCalls, "ENI must be detached before delete")
	assert.Equal(t, []string{"eni-1234"}, ed.calls)
}

func TestTerminateStoppedInstance_ENIDeleteNotFoundTolerated(t *testing.T) {
	// A retried teardown after the ENI is already gone returns NotFound. The
	// instance must still finalize to the terminated bucket, not error out.
	id := "i-eni-gone"
	v := &vm.VM{ID: id, Status: vm.StateStopped, AccountID: "acc", ENIId: "eni-gone"}
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{id: v}}
	ed := &fakeENIDeleter{err: errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)}
	svc := &InstanceServiceImpl{stoppedStore: store, eniDeleter: ed, eniCreator: &fakeENICreator{}}

	out, err := svc.TerminateStoppedInstance(context.Background(), &TerminateStoppedInstanceInput{InstanceID: id}, "acc")
	require.NoError(t, err)
	assert.Equal(t, "terminated", out.Status)
	assert.Equal(t, []string{"eni-gone"}, ed.calls)
}

// --- StartStoppedInstance tests ---

type fakeGPUClaimer struct {
	claimed    []string
	released   []string
	claimErr   error
	attachment gpu.GPUAttachment
}

func (f *fakeGPUClaimer) Claim(instanceID, _ string) (*gpu.GPUAttachment, error) {
	f.claimed = append(f.claimed, instanceID)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	att := f.attachment
	return &att, nil
}

func (f *fakeGPUClaimer) Release(instanceID string) error {
	f.released = append(f.released, instanceID)
	return nil
}

func TestStartStoppedInstance_MissingInstanceID(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestStartStoppedInstance_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: "i-1"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestStartStoppedInstance_NilResourceMgr(t *testing.T) {
	svc := &InstanceServiceImpl{stoppedStore: &fakeStoppedStore{loadByID: map[string]*vm.VM{}}}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: "i-1"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestStartStoppedInstance_NilVMMgr(t *testing.T) {
	svc := &InstanceServiceImpl{
		stoppedStore: &fakeStoppedStore{loadByID: map[string]*vm.VM{}},
		resourceMgr:  &fakeResourceCapacityProvider{},
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: "i-1"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestStartStoppedInstance_LoadError(t *testing.T) {
	store := &fakeStoppedStore{loadErr: errors.New("kv down")}
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  &fakeResourceCapacityProvider{},
		vmMgr:        vm.NewManager(),
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: "i-1"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestStartStoppedInstance_NotFound(t *testing.T) {
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{}}
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  &fakeResourceCapacityProvider{},
		vmMgr:        vm.NewManager(),
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: "i-missing"}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestStartStoppedInstance_NotStopped(t *testing.T) {
	id := "i-running"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateRunning, AccountID: "acc"},
	}}
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  &fakeResourceCapacityProvider{},
		vmMgr:        vm.NewManager(),
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestStartStoppedInstance_NotVisible(t *testing.T) {
	id := "i-foreign"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "owner-acc", InstanceType: "t3.micro"},
	}}
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  &fakeResourceCapacityProvider{},
		vmMgr:        vm.NewManager(),
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, "other-acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())

	// Cross-tenant rejection must not delete from KV.
	assert.Empty(t, store.deletedStopped, "cross-tenant rejection must not delete stopped instance")
}

func TestStartStoppedInstance_InstanceTypeUnknown(t *testing.T) {
	id := "i-badtype"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc", InstanceType: "z99.nope"},
	}}
	prov := &fakeResourceCapacityProvider{instanceTypes: map[string]*ec2.InstanceTypeInfo{}}
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  prov,
		vmMgr:        vm.NewManager(),
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
	assert.Empty(t, prov.allocated, "no allocation should occur for unknown type")
}

func TestStartStoppedInstance_AllocateFails(t *testing.T) {
	id := "i-alloc-fail"
	itype := "t3.micro"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc", InstanceType: itype},
	}}
	prov := &fakeResourceCapacityProvider{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{
			itype: {InstanceType: aws.String(itype)},
		},
		allocateErr: errors.New("no capacity"),
	}
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  prov,
		vmMgr:        vm.NewManager(),
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

// GPU claim failure must roll back the resource allocation and remove the VM
// from the manager's map — otherwise stale capacity stays consumed.
func TestStartStoppedInstance_GPUClaimFailureRollsBack(t *testing.T) {
	id := "i-gpu-fail"
	itype := "g5.xlarge"
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, AccountID: "acc", InstanceType: itype},
	}}
	prov := &fakeResourceCapacityProvider{
		instanceTypes: map[string]*ec2.InstanceTypeInfo{
			itype: {InstanceType: aws.String(itype), GpuInfo: &ec2.GpuInfo{
				Gpus: []*ec2.GpuDeviceInfo{{Name: aws.String("nvidia-a10g"), Count: aws.Int64(1)}},
			}},
		},
	}
	claimer := &fakeGPUClaimer{claimErr: errors.New("vfio bind failed")}
	mgr := vm.NewManager()
	svc := &InstanceServiceImpl{
		stoppedStore: store,
		resourceMgr:  prov,
		vmMgr:        mgr,
		gpuClaimer:   claimer,
	}
	_, err := svc.StartStoppedInstance(context.Background(), &StartStoppedInstanceInput{InstanceID: id}, "acc")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())

	require.Len(t, prov.allocated, 1, "allocator must have been called")
	require.Len(t, prov.deallocated, 1, "GPU claim failure must trigger deallocate")
	_, stillInMgr := mgr.Get(id)
	assert.False(t, stillInMgr, "GPU claim failure must remove the VM from the manager map")
	assert.Empty(t, store.deletedStopped, "stopped-KV entry must remain on rollback")
}

// --- PrepareRunInstances / ec2.cmd dispatch tests ---------------------------

type fakeAMILoader struct {
	byID map[string]viperblock.AMIMetadata
	err  error
}

func (f *fakeAMILoader) GetAMIConfig(_ context.Context, id string) (viperblock.AMIMetadata, error) {
	if f.err != nil {
		return viperblock.AMIMetadata{}, f.err
	}
	if meta, ok := f.byID[id]; ok {
		return meta, nil
	}
	return viperblock.AMIMetadata{}, errors.New("not found")
}

type fakeKeyValidator struct {
	err error
}

func (f *fakeKeyValidator) ValidateKeyPairExists(_ context.Context, _ string, _ string) error {
	return f.err
}

func defaultPrepareInstanceTypes() (map[string]*ec2.InstanceTypeInfo, *ec2.InstanceTypeInfo) {
	it := &ec2.InstanceTypeInfo{InstanceType: aws.String("t3.micro")}
	return map[string]*ec2.InstanceTypeInfo{"t3.micro": it}, it
}

func TestPrepareRunInstances_MissingAccountID(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{InstanceType: aws.String("t3.micro")}, "", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestPrepareRunInstances_MissingInstanceType(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestPrepareRunInstances_InvalidInstanceType(t *testing.T) {
	svc := &InstanceServiceImpl{instanceTypes: map[string]*ec2.InstanceTypeInfo{}}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("unknown.type"),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceType, err.Error())
}

func TestPrepareRunInstances_MissingImageID(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	svc := &InstanceServiceImpl{instanceTypes: types}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestPrepareRunInstances_NilAMILoader(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	svc := &InstanceServiceImpl{instanceTypes: types}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestPrepareRunInstances_AMINotFound(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	svc := &InstanceServiceImpl{
		instanceTypes: types,
		amiLoader:     &fakeAMILoader{err: errors.New("missing")},
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-missing"),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestPrepareRunInstances_AMINotOwnedByCaller(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	svc := &InstanceServiceImpl{
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-other": {ImageOwnerAlias: "999988887777"},
		}},
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-other"),
	}, "111122223333", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestPrepareRunInstances_KeyPairNotFound(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	svc := &InstanceServiceImpl{
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		keyValidator: &fakeKeyValidator{err: errors.New("no key")},
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		KeyName:      aws.String("nope"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

func TestPrepareRunInstances_InsufficientCapacity(t *testing.T) {
	types, it := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		canAllocFn:    func(*ec2.InstanceTypeInfo, int) int { return 0 },
	}
	_ = it
	svc := &InstanceServiceImpl{
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(5),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestPrepareRunInstances_HappyPathNoENI(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	reservation, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}, "acc", "")
	require.NoError(t, err)
	require.NotNil(t, reservation)
	require.Len(t, instances, 2)
	require.Len(t, reservation.Instances, 2)
	assert.Equal(t, "acc", *reservation.OwnerId)
	for _, inst := range instances {
		assert.Equal(t, "acc", inst.AccountID)
		assert.Equal(t, "t3.micro", inst.InstanceType)
	}
}

// An instance profile passed at launch is persisted on every prepared VM and
// echoed on the ec2.Instance so the in-VM IMDS endpoint resolves a role and
// serves its credentials (mirrors the RunInstance singular path).
func TestPrepareRunInstances_PersistsIamInstanceProfile(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	const profileARN = "arn:aws:iam::000000000000:instance-profile/spinifex-eks-server"
	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType:       aws.String("t3.micro"),
		ImageId:            aws.String("ami-1"),
		MinCount:           aws.Int64(2),
		MaxCount:           aws.Int64(2),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Arn: aws.String(profileARN)},
	}, "acc", "")
	require.NoError(t, err)
	require.Len(t, instances, 2)
	for _, inst := range instances {
		assert.Equal(t, profileARN, inst.IamInstanceProfileArn)
		assert.NotEmpty(t, inst.IamInstanceProfileAssociationId)
		require.NotNil(t, inst.Instance.IamInstanceProfile)
		assert.Equal(t, profileARN, aws.StringValue(inst.Instance.IamInstanceProfile.Arn))
	}
}

// A targeted launch consumes slots from the reservation (not the general pool)
// and stamps the reservation id onto every prepared VM so terminate can restore.
func TestPrepareRunInstances_ConsumesReservation(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes:      types,
		reservationAvailFn: func(_, _ string, _ *ec2.InstanceTypeInfo) int { return 3 },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}, "acc", "cr-123")
	require.NoError(t, err)
	require.Len(t, instances, 2)
	assert.Len(t, prov.reservationAllocated, 2, "slots consumed from the reservation")
	assert.Empty(t, prov.allocated, "targeted launch must not touch general capacity")
	for _, inst := range instances {
		assert.Equal(t, "cr-123", inst.CapacityReservationId, "instance stamped with reservation id")
	}
}

// A targeted launch is capped at the reservation's free slots and never spills
// onto general capacity: MaxCount past Available yields exactly Available.
func TestPrepareRunInstances_ReservationCapsNoSpill(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes:      types,
		reservationAvailFn: func(_, _ string, _ *ec2.InstanceTypeInfo) int { return 2 },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(5),
	}, "acc", "cr-123")
	require.NoError(t, err)
	assert.Len(t, instances, 2, "launch capped at the 2 available reservation slots")
	assert.Empty(t, prov.allocated, "no general-pool allocation for a targeted launch")
}

// When the reservation has fewer free slots than MinCount the launch is rejected
// with ReservationCapacityExceeded and nothing is allocated.
func TestPrepareRunInstances_ReservationExceeded(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes:      types,
		reservationAvailFn: func(_, _ string, _ *ec2.InstanceTypeInfo) int { return 1 },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}, "acc", "cr-123")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorReservationCapacityExceeded, err.Error())
	assert.Empty(t, prov.reservationAllocated, "nothing allocated when below MinCount")
}

// Invariant guardrail: a mid-launch failure on the reservation path must return
// every consumed slot to the reservation (reservationReleased == reservationAllocated)
// and never leak one onto the general pool (Deallocate untouched). Each per-instance
// ENI create fails, exercising a rollback site inside the launch loop.
func TestPrepareRunInstances_ReservationRollbackNoGeneralPoolLeak(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes:      types,
		reservationAvailFn: func(_, _ string, _ *ec2.InstanceTypeInfo) int { return 2 },
	}
	eni := &fakeENICreator{createErr: errors.New("eni create failed")} // fails every call
	svc := &InstanceServiceImpl{
		config:        &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
		eniCreator:  eni,
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		SubnetId:     aws.String("subnet-1"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}, "acc", "cr-123")
	require.Error(t, err)
	assert.Len(t, prov.reservationAllocated, 2, "both slots allocated from the reservation")
	assert.Len(t, prov.reservationReleased, 2, "both reservation slots restored on rollback")
	assert.Empty(t, prov.deallocated, "no reservation-bound slot may leak to the general pool")
}

// TestPrepareRunInstances_AmiLaunchIndexContiguous pins that ami-launch-index is
// assigned per successful launch (0..n-1) so it flows to DescribeInstances and the
// IMDS identity document. Survivors of a mid-loop failure stay contiguous (no gap).
func TestPrepareRunInstances_AmiLaunchIndexContiguous(t *testing.T) {
	t.Run("count_3_no_eni", func(t *testing.T) {
		types, _ := defaultPrepareInstanceTypes()
		prov := &fakeResourceCapacityProvider{
			instanceTypes: types,
			canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
		}
		svc := &InstanceServiceImpl{
			config:        &config.Config{},
			instanceTypes: types,
			amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
				"ami-1": {ImageOwnerAlias: "acc"},
			}},
			resourceMgr: prov,
		}
		reservation, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
			InstanceType: aws.String("t3.micro"),
			ImageId:      aws.String("ami-1"),
			MinCount:     aws.Int64(3),
			MaxCount:     aws.Int64(3),
		}, "acc", "")
		require.NoError(t, err)
		require.Len(t, instances, 3)
		require.Len(t, reservation.Instances, 3)
		for i, inst := range reservation.Instances {
			assert.Equal(t, int64(i), aws.Int64Value(inst.AmiLaunchIndex))
		}
	})

	t.Run("mid_loop_failure_stays_contiguous", func(t *testing.T) {
		// Second of three ENI creates fails, so the second instance never
		// appends; the third fills index 1 (not 2), leaving no gap.
		eni := &fakeENICreator{
			defaultSubnet: &SubnetInfo{SubnetID: "subnet-1", VpcID: "vpc-1"},
			createOut: &ec2.CreateNetworkInterfaceOutput{
				NetworkInterface: &ec2.NetworkInterface{
					NetworkInterfaceId: aws.String("eni-1"),
					MacAddress:         aws.String("aa:bb:cc:dd:ee:ff"),
					PrivateIpAddress:   aws.String("10.0.0.10"),
					VpcId:              aws.String("vpc-1"),
				},
			},
			createErr:       errors.New("eni create failed"),
			createErrOnCall: 2,
		}
		svc, _ := prepareSvcWithENI(t, eni, nil)

		reservation, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
			InstanceType: aws.String("t3.micro"),
			ImageId:      aws.String("ami-1"),
			SubnetId:     aws.String("subnet-1"),
			MinCount:     aws.Int64(2),
			MaxCount:     aws.Int64(3),
		}, "acc", "")
		require.NoError(t, err)
		require.Len(t, instances, 2)
		require.Len(t, reservation.Instances, 2)
		assert.Equal(t, int64(0), aws.Int64Value(reservation.Instances[0].AmiLaunchIndex))
		assert.Equal(t, int64(1), aws.Int64Value(reservation.Instances[1].AmiLaunchIndex))
	})
}

// TestPrepareRunInstances_BootModePropagated pins that the AMI's BootMode
// flows onto every prepared VM, so the launch path picks UEFI vs BIOS without
// a second AMI lookup. Empty AMI BootMode (legacy) flows through as empty.
func TestPrepareRunInstances_BootModePropagated(t *testing.T) {
	tests := []struct {
		name         string
		amiBootMode  string
		wantBootMode string
	}{
		{"legacy empty", "", ""},
		{"bios", "bios", "bios"},
		{"uefi", "uefi", "uefi"},
		{"uefi-preferred", "uefi-preferred", "uefi-preferred"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			types, _ := defaultPrepareInstanceTypes()
			prov := &fakeResourceCapacityProvider{
				instanceTypes: types,
				canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
			}
			svc := &InstanceServiceImpl{
				config:        &config.Config{},
				instanceTypes: types,
				amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
					"ami-1": {ImageOwnerAlias: "acc", BootMode: tc.amiBootMode},
				}},
				resourceMgr: prov,
			}
			_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
				InstanceType: aws.String("t3.micro"),
				ImageId:      aws.String("ami-1"),
				MinCount:     aws.Int64(1),
				MaxCount:     aws.Int64(1),
			}, "acc", "")
			require.NoError(t, err)
			require.Len(t, instances, 1)
			assert.Equal(t, tc.wantBootMode, instances[0].BootMode)
		})
	}
}

func TestStartInstance_NotStopped(t *testing.T) {
	id := "i-running"
	mgr := mgrWith(map[string]*vm.VM{id: {ID: id, Status: vm.StateRunning}})
	v, _ := mgr.Get(id)
	svc := &InstanceServiceImpl{vmMgr: mgr}
	err := svc.StartInstance(context.Background(), v, spxtypes.EC2InstanceCommand{ID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestStopOrTerminateInstance_TerminateIdempotent(t *testing.T) {
	id := "i-shutting"
	mgr := mgrWith(map[string]*vm.VM{id: {ID: id, Status: vm.StateShuttingDown}})
	v, _ := mgr.Get(id)
	svc := &InstanceServiceImpl{vmMgr: mgr}
	err := svc.StopOrTerminateInstance(context.Background(), v, spxtypes.EC2InstanceCommand{
		ID:         id,
		Attributes: spxtypes.EC2CommandAttributes{TerminateInstance: true},
	})
	require.NoError(t, err)
}

func TestStopOrTerminateInstance_InvalidTransition(t *testing.T) {
	id := "i-stopped"
	mgr := mgrWith(map[string]*vm.VM{id: {ID: id, Status: vm.StateStopped}})
	v, _ := mgr.Get(id)
	svc := &InstanceServiceImpl{vmMgr: mgr}
	err := svc.StopOrTerminateInstance(context.Background(), v, spxtypes.EC2InstanceCommand{
		ID:         id,
		Attributes: spxtypes.EC2CommandAttributes{StopInstance: true},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestStopOrTerminateInstance_TerminationProtection(t *testing.T) {
	// DisableApiTermination must block Terminate but never Stop.
	tests := []struct {
		name    string
		attrs   spxtypes.EC2CommandAttributes
		wantErr string
	}{
		{name: "terminate blocked", attrs: spxtypes.EC2CommandAttributes{TerminateInstance: true}, wantErr: awserrors.ErrorOperationNotPermitted},
		{name: "stop allowed", attrs: spxtypes.EC2CommandAttributes{StopInstance: true}, wantErr: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := "i-prot"
			mgr := mgrWith(map[string]*vm.VM{
				id: {
					ID: id, Status: vm.StateRunning,
					RunInstancesInput: &ec2.RunInstancesInput{DisableApiTermination: aws.Bool(true)},
				},
			})
			v, _ := mgr.Get(id)
			svc := &InstanceServiceImpl{vmMgr: mgr}

			err := svc.StopOrTerminateInstance(context.Background(), v, spxtypes.EC2InstanceCommand{ID: id, Attributes: tc.attrs})
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
			assert.Equal(t, vm.StateRunning, v.Status, "VM state must not change when blocked")
		})
	}
}

type fakeENICreator struct {
	defaultSubnet   *SubnetInfo
	subnet          *SubnetInfo
	getENIByID      map[string]*ENIInfo
	getENIErr       error
	createOut       *ec2.CreateNetworkInterfaceOutput
	createCalls     int
	createErr       error
	createErrOnCall int // 1-based call index to fail; 0 fails every call when createErr is set
	attachErr       error
	attachCalls     int
	updateCalls     int
	clearCalls      int // updateCalls where publicIP is ""
	detachCalls     int
}

func (f *fakeENICreator) GetDefaultSubnet(_ context.Context, _ string) (*SubnetInfo, error) {
	if f.defaultSubnet == nil {
		return nil, errors.New("no default")
	}
	return f.defaultSubnet, nil
}

func (f *fakeENICreator) GetSubnet(_ context.Context, _, _ string) (*SubnetInfo, error) {
	if f.subnet == nil {
		return nil, errors.New("no subnet")
	}
	return f.subnet, nil
}

func (f *fakeENICreator) GetENI(_ context.Context, _, eniID string) (*ENIInfo, error) {
	if f.getENIErr != nil {
		return nil, f.getENIErr
	}
	if f.getENIByID == nil {
		return nil, errors.New("no ENI configured")
	}
	info, ok := f.getENIByID[eniID]
	if !ok {
		return nil, errors.New("eni not found")
	}
	return info, nil
}

func (f *fakeENICreator) CreateNetworkInterface(_ context.Context, _ *ec2.CreateNetworkInterfaceInput, _ string) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.createCalls++
	if f.createErr != nil && (f.createErrOnCall == 0 || f.createErrOnCall == f.createCalls) {
		return nil, f.createErr
	}
	return f.createOut, nil
}

func (f *fakeENICreator) AttachENI(_ context.Context, _, _, _ string, _ int64) (string, error) {
	f.attachCalls++
	if f.attachErr != nil {
		return "", f.attachErr
	}
	return "attached", nil
}

func (f *fakeENICreator) DetachENI(_ context.Context, _, _ string) error {
	f.detachCalls++
	return nil
}

func (f *fakeENICreator) UpdateENIPublicIP(_ context.Context, _, _, publicIP, _ string) error {
	f.updateCalls++
	if publicIP == "" {
		f.clearCalls++
	}
	return nil
}

type fakeIPAllocator struct {
	publicIP string
	poolName string
	err      error
}

func (f *fakeIPAllocator) AllocateIP(_ context.Context, _, _, _, _, _, _ string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.publicIP, f.poolName, nil
}

// prepareSvcWithENI returns a service wired with allocator + AMI + ENI/IP deps
// suitable for happy-path PrepareRunInstances tests.
func prepareSvcWithENI(t *testing.T, eni *fakeENICreator, ipam *fakeIPAllocator) (*InstanceServiceImpl, *fakeResourceCapacityProvider) {
	t.Helper()
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
		eniCreator:  eni,
		ipAllocator: ipam,
		natsConn:    embeddedNATS(t),
	}
	return svc, prov
}

func TestPrepareRunInstances_DefaultSubnetResolved(t *testing.T) {
	eni := &fakeENICreator{
		defaultSubnet: &SubnetInfo{SubnetID: "subnet-default", VpcID: "vpc-1"},
		createOut: &ec2.CreateNetworkInterfaceOutput{
			NetworkInterface: &ec2.NetworkInterface{
				NetworkInterfaceId: aws.String("eni-1"),
				MacAddress:         aws.String("aa:bb:cc:dd:ee:ff"),
				PrivateIpAddress:   aws.String("10.0.0.10"),
				VpcId:              aws.String("vpc-1"),
			},
		},
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)

	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, "eni-1", instances[0].ENIId)
	assert.Equal(t, 1, eni.attachCalls)
}

func TestPrepareRunInstances_PublicIPAutoAssigned(t *testing.T) {
	cases := []struct {
		name        string
		mapOnLaunch bool
		nicOverride *bool
		wantPublic  bool
	}{
		{name: "subnet_true_no_override", mapOnLaunch: true, nicOverride: nil, wantPublic: true},
		{name: "subnet_false_override_true", mapOnLaunch: false, nicOverride: aws.Bool(true), wantPublic: true},
		{name: "subnet_true_override_false", mapOnLaunch: true, nicOverride: aws.Bool(false), wantPublic: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eni := &fakeENICreator{
				subnet: &SubnetInfo{SubnetID: "subnet-1", VpcID: "vpc-1", MapPublicIpOnLaunch: tc.mapOnLaunch},
				createOut: &ec2.CreateNetworkInterfaceOutput{
					NetworkInterface: &ec2.NetworkInterface{
						NetworkInterfaceId: aws.String("eni-2"),
						MacAddress:         aws.String("aa:bb:cc:dd:ee:00"),
						PrivateIpAddress:   aws.String("10.0.0.20"),
						VpcId:              aws.String("vpc-1"),
					},
				},
			}
			ipam := &fakeIPAllocator{publicIP: "203.0.113.5", poolName: "pool-a"}
			svc, _ := prepareSvcWithENI(t, eni, ipam)

			// vpc.add-nat is now request-reply: the helper waits for vpcd to
			// ack before the launch proceeds. The happy path needs a success
			// responder.
			sub, err := svc.natsConn.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
				_ = msg.Respond([]byte(`{"success":true}`))
			})
			require.NoError(t, err)
			defer func() { _ = sub.Unsubscribe() }()

			input := &ec2.RunInstancesInput{
				InstanceType: aws.String("t3.micro"),
				ImageId:      aws.String("ami-1"),
				SubnetId:     aws.String("subnet-1"),
				MinCount:     aws.Int64(1),
				MaxCount:     aws.Int64(1),
			}
			if tc.nicOverride != nil {
				input.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
					{AssociatePublicIpAddress: tc.nicOverride},
				}
			}

			_, instances, _, err := svc.PrepareRunInstances(context.Background(), input, "acc", "")
			require.NoError(t, err)
			require.Len(t, instances, 1)
			if tc.wantPublic {
				assert.Equal(t, "203.0.113.5", instances[0].PublicIP)
				assert.Equal(t, "pool-a", instances[0].PublicIPPool)
				assert.Equal(t, 1, eni.updateCalls)
			} else {
				assert.Empty(t, instances[0].PublicIP)
				assert.Empty(t, instances[0].PublicIPPool)
				assert.Equal(t, 0, eni.updateCalls)
			}
		})
	}
}

// TestPrepareRunInstances_NATFailureRollsBackPublicIP verifies that a vpc.add-nat
// failure drops the instance, clears the ENI public IP, releases the IPAM
// lease, and deallocates capacity.
func TestPrepareRunInstances_NATFailureRollsBackPublicIP(t *testing.T) {
	eni := &fakeENICreator{
		subnet: &SubnetInfo{SubnetID: "subnet-1", VpcID: "vpc-1", MapPublicIpOnLaunch: true},
		createOut: &ec2.CreateNetworkInterfaceOutput{
			NetworkInterface: &ec2.NetworkInterface{
				NetworkInterfaceId: aws.String("eni-nat-fail"),
				MacAddress:         aws.String("aa:bb:cc:dd:ee:ff"),
				PrivateIpAddress:   aws.String("10.0.0.30"),
				VpcId:              aws.String("vpc-1"),
			},
		},
	}
	ipam := &fakeIPAllocator{publicIP: "203.0.113.7", poolName: "pool-a"}
	releaser := &fakePublicIPReleaser{}
	deleter := &fakeENIDeleter{}
	svc, prov := prepareSvcWithENI(t, eni, ipam)
	svc.ipReleaser = releaser
	svc.eniDeleter = deleter

	// Stand up a vpcd-shaped responder that NACKs every add-nat — simulates
	// vpcd online but unable to commit the OVN rule (northd lag, port
	// missing, etc.).
	sub, err := svc.natsConn.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"success":false,"error":"northd unavailable"}`))
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	// Record vpc.delete-nat to assert the rollback neutralises any
	// half-committed rule from a waitForFlowsHV timeout.
	deleteNATCh := make(chan natWirePayload, 1)
	delSub, err := svc.natsConn.Subscribe("vpc.delete-nat", func(msg *nats.Msg) {
		var p natWirePayload
		_ = json.Unmarshal(msg.Data, &p)
		select {
		case deleteNATCh <- p:
		default:
		}
	})
	require.NoError(t, err)
	defer func() { _ = delSub.Unsubscribe() }()

	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		SubnetId:     aws.String("subnet-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")

	// MinCount=1 with a single launch attempt that NAT-failed must drop
	// below minimum and return ServerInternal — the wrapped natErr is not
	// an AWS-known code, so the lookup misses and the wire fallback kicks
	// in. The critical invariant is that no instance with the unreachable
	// IP is surfaced.
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, instances, "instance with failed NAT must not be returned")

	// IPAM lease released back to the pool.
	assert.Equal(t, "pool-a", releaser.pool, "IPAM ReleaseIP must be called with the originally allocated pool")
	assert.Equal(t, "203.0.113.7", releaser.ip, "IPAM ReleaseIP must be called with the originally allocated IP")

	// ENI record cleared: 2 update calls total (set + clear), 1 clear.
	assert.Equal(t, 2, eni.updateCalls, "ENI public IP should be set once, then cleared on rollback")
	assert.Equal(t, 1, eni.clearCalls, "ENI rollback should call UpdateENIPublicIP with empty publicIP")

	// ENI itself must be detached + deleted; otherwise the dropped instance
	// leaks an eniKV record (vmMgr.Insert never runs, so terminateCleanup
	// never fires) and the subnet exhausts private IPs under vpcd brown-out.
	assert.Equal(t, 1, eni.detachCalls, "rollback must detach the ENI before delete")
	assert.Equal(t, []string{"eni-nat-fail"}, deleter.calls, "rollback must delete the auto-created ENI")

	// Capacity deallocated so subsequent launches can use the slot.
	require.Len(t, prov.deallocated, 1, "NAT failure must trigger Deallocate")

	// vpc.delete-nat must be published so vpcd reaps any rule it committed
	// after the AddNAT timeout — otherwise the orphan rule outlives the
	// IPAM allocation and routes traffic for the next tenant that grabs
	// the released IP.
	select {
	case got := <-deleteNATCh:
		assert.Equal(t, "vpc-1", got.VpcId)
		assert.Equal(t, "203.0.113.7", got.ExternalIP)
		assert.Equal(t, "10.0.0.30", got.LogicalIP)
		assert.Equal(t, "port-eni-nat-fail", got.PortName)
	case <-time.After(time.Second):
		t.Fatal("rollback must publish vpc.delete-nat to neutralise a half-committed rule")
	}
}

// TestPrepareRunInstances_PublicIPAllocFailureAbortsLaunch verifies that a
// failed public-IP allocation aborts the launch: detaches and deletes the ENI,
// deallocates capacity, and returns InsufficientAddressCapacity.
func TestPrepareRunInstances_PublicIPAllocFailureAbortsLaunch(t *testing.T) {
	eni := &fakeENICreator{
		subnet: &SubnetInfo{SubnetID: "subnet-1", VpcID: "vpc-1", MapPublicIpOnLaunch: true},
		createOut: &ec2.CreateNetworkInterfaceOutput{
			NetworkInterface: &ec2.NetworkInterface{
				NetworkInterfaceId: aws.String("eni-pubip-fail"),
				MacAddress:         aws.String("aa:bb:cc:dd:ee:02"),
				PrivateIpAddress:   aws.String("10.0.0.50"),
				VpcId:              aws.String("vpc-1"),
			},
		},
	}
	ipam := &fakeIPAllocator{err: errors.New("InsufficientAddressCapacity: pool wan exhausted")}
	deleter := &fakeENIDeleter{}
	svc, prov := prepareSvcWithENI(t, eni, ipam)
	svc.eniDeleter = deleter

	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		SubnetId:     aws.String("subnet-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientAddressCapacity, err.Error(),
		"pool exhaustion must surface as InsufficientAddressCapacity, not a silent IP-less boot")
	assert.Empty(t, instances, "instance with no public IP must not be returned")

	// The ENI is attached with a primary IP; rollback must detach before
	// delete (in-use ENIs reject deletion) so no eniKV record leaks.
	assert.Equal(t, 1, eni.detachCalls, "rollback must detach the ENI before delete")
	assert.Equal(t, []string{"eni-pubip-fail"}, deleter.calls, "rollback must delete the auto-created ENI")
	assert.Equal(t, 0, eni.updateCalls, "no public IP was allocated, so the ENI must never be updated with one")

	require.Len(t, prov.deallocated, 1, "public-IP allocation failure must trigger Deallocate")
}

// natWirePayload mirrors utils.natEvent for test-side decoding.
type natWirePayload struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"`
	MAC        string `json:"mac"`
}

func TestPrepareRunInstances_ENICreateFailureDeallocates(t *testing.T) {
	eni := &fakeENICreator{createErr: errors.New("boom")}
	svc, prov := prepareSvcWithENI(t, eni, nil)

	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		SubnetId:     aws.String("subnet-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")
	require.Error(t, err)
	// MinCount not satisfied → InsufficientInstanceCapacity (no known
	// AWS code for raw ENI failure).
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	require.Len(t, prov.deallocated, 1, "ENI failure must trigger deallocate")
}

// TestPrepareRunInstances_ENIAttachFailureRollsBack verifies that an AttachENI
// failure deletes the auto-created ENI, deallocates capacity, and drops the
// instance from the reservation.
func TestPrepareRunInstances_ENIAttachFailureRollsBack(t *testing.T) {
	eni := &fakeENICreator{
		attachErr: errors.New("vpc attach refused"),
		createOut: &ec2.CreateNetworkInterfaceOutput{
			NetworkInterface: &ec2.NetworkInterface{
				NetworkInterfaceId: aws.String("eni-attach-fail"),
				MacAddress:         aws.String("aa:bb:cc:dd:ee:01"),
				PrivateIpAddress:   aws.String("10.0.0.40"),
				VpcId:              aws.String("vpc-1"),
			},
		},
	}
	deleter := &fakeENIDeleter{}
	svc, prov := prepareSvcWithENI(t, eni, nil)
	svc.eniDeleter = deleter

	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		SubnetId:     aws.String("subnet-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, instances, "instance with failed AttachENI must not be returned")

	assert.Equal(t, 1, eni.attachCalls, "AttachENI must be attempted exactly once")
	assert.Equal(t, []string{"eni-attach-fail"}, deleter.calls, "auto-created ENI must be deleted on attach failure")
	require.Len(t, prov.deallocated, 1, "AttachENI failure must trigger Deallocate")
}

func TestPrepareRunInstances_NetworkInterfaceLifted(t *testing.T) {
	// Terraform-style: subnet+SG come via NetworkInterfaces[0], not top-level.
	eni := &fakeENICreator{
		createOut: &ec2.CreateNetworkInterfaceOutput{
			NetworkInterface: &ec2.NetworkInterface{
				NetworkInterfaceId: aws.String("eni-3"),
				MacAddress:         aws.String("aa:bb:cc:dd:ee:11"),
				PrivateIpAddress:   aws.String("10.0.0.30"),
				VpcId:              aws.String("vpc-1"),
			},
		},
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)

	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{
			{
				SubnetId: aws.String("subnet-tf"),
				Groups:   []*string{aws.String("sg-1")},
			},
		},
	}, "acc", "")
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, "eni-3", instances[0].ENIId)
}

func TestPrepareRunInstances_PlacementGroup(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		Placement:    &ec2.Placement{GroupName: aws.String("pg-1")},
	}, "acc", "")
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, "pg-1", instances[0].PlacementGroupName)
}

func TestPrepareRunInstances_AllocateFailsMidLoop(t *testing.T) {
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		canAllocFn:    func(_ *ec2.InstanceTypeInfo, count int) int { return count },
		allocateErr:   errors.New("oom"),
	}
	svc := &InstanceServiceImpl{
		config:        &config.Config{},
		instanceTypes: types,
		amiLoader: &fakeAMILoader{byID: map[string]viperblock.AMIMetadata{
			"ami-1": {ImageOwnerAlias: "acc"},
		}},
		resourceMgr: prov,
	}
	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(3),
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestStartInstance_AllocateFails(t *testing.T) {
	id := "i-2"
	mgr := mgrWith(map[string]*vm.VM{
		id: {ID: id, Status: vm.StateStopped, InstanceType: "t3.micro"},
	})
	v, _ := mgr.Get(id)
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		allocateErr:   errors.New("no capacity"),
	}
	svc := &InstanceServiceImpl{vmMgr: mgr, resourceMgr: prov}
	err := svc.StartInstance(context.Background(), v, spxtypes.EC2InstanceCommand{ID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

// TestStartInstance_ErrorStateStartable verifies a StateError instance passes
// the startability guard and reaches resource allocation.
func TestStartInstance_ErrorStateStartable(t *testing.T) {
	id := "i-err"
	mgr := mgrWith(map[string]*vm.VM{
		id: {ID: id, Status: vm.StateError, InstanceType: "t3.micro"},
	})
	v, _ := mgr.Get(id)
	types, _ := defaultPrepareInstanceTypes()
	prov := &fakeResourceCapacityProvider{
		instanceTypes: types,
		allocateErr:   errors.New("no capacity"),
	}
	svc := &InstanceServiceImpl{vmMgr: mgr, resourceMgr: prov}
	err := svc.StartInstance(context.Background(), v, spxtypes.EC2InstanceCommand{ID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestRebootInstance_NotFound(t *testing.T) {
	id := "i-missing"
	mgr := mgrWith(nil)
	svc := &InstanceServiceImpl{vmMgr: mgr}
	err := svc.RebootInstance(context.Background(), &vm.VM{ID: id}, spxtypes.EC2InstanceCommand{ID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// TestStartInstance_NotFound verifies that a missing instance returns
// InvalidInstanceID.NotFound rather than a generic internal error.
func TestStartInstance_NotFound(t *testing.T) {
	id := "i-missing"
	mgr := mgrWith(nil)
	svc := &InstanceServiceImpl{
		vmMgr:       mgr,
		resourceMgr: &fakeResourceCapacityProvider{},
	}
	instance := &vm.VM{ID: id, Status: vm.StateStopped, InstanceType: "unknown"}
	err := svc.StartInstance(context.Background(), instance, spxtypes.EC2InstanceCommand{ID: id})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// TestRunInstances_PrepareError covers the InstanceService-level RunInstances
// (sync convenience method) error propagation when PrepareRunInstances rejects.
func TestRunInstances_PrepareError(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.RunInstances(context.Background(), &ec2.RunInstancesInput{}, "acc")
	require.Error(t, err)
}

// --- DescribeInstanceStatus ---

func instanceStatusService(t *testing.T, az string, vms map[string]*vm.VM) *InstanceServiceImpl {
	t.Helper()
	return &InstanceServiceImpl{
		config: &config.Config{AZ: az},
		vmMgr:  mgrWith(vms),
	}
}

func runningVM(id, owner string) *vm.VM {
	return &vm.VM{
		ID:        id,
		Status:    vm.StateRunning,
		AccountID: owner,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-" + id),
			OwnerId:       aws.String(owner),
		},
		Instance: &ec2.Instance{InstanceId: aws.String(id)},
	}
}

func TestDescribeInstanceStatus_Empty(t *testing.T) {
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{})
	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, utils.GlobalAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_RunningInstance(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-aaa", owner)
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)

	s := out.InstanceStatuses[0]
	assert.Equal(t, "i-aaa", *s.InstanceId)
	assert.Equal(t, "az-a", *s.AvailabilityZone)
	assert.Equal(t, "running", *s.InstanceState.Name)
	assert.Equal(t, int64(16), *s.InstanceState.Code)
	assert.Equal(t, "ok", *s.InstanceStatus.Status)
	assert.Equal(t, "ok", *s.SystemStatus.Status)
	require.Len(t, s.InstanceStatus.Details, 1)
	assert.Equal(t, "passed", *s.InstanceStatus.Details[0].Status)
}

func TestDescribeInstanceStatus_AccountFilteringHidesOtherTenant(t *testing.T) {
	v := runningVM("i-other", "999988887777")
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, "111122223333")
	require.NoError(t, err)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_StoppedExcludedByDefault(t *testing.T) {
	owner := "111122223333"
	stopped := runningVM("i-stop", owner)
	stopped.Status = vm.StateStopped
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{stopped.ID: stopped})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_IncludeAllSurfacesPending(t *testing.T) {
	owner := "111122223333"
	pend := runningVM("i-pend", owner)
	pend.Status = vm.StatePending
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{pend.ID: pend})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
	}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "pending", *out.InstanceStatuses[0].InstanceState.Name)
	assert.Equal(t, "not-applicable", *out.InstanceStatuses[0].InstanceStatus.Status)
	assert.Equal(t, "not-applicable", *out.InstanceStatuses[0].SystemStatus.Status)
}

func TestDescribeInstanceStatus_IncludeAllSurfacesStopped(t *testing.T) {
	owner := "111122223333"
	stopped := runningVM("i-stop", owner)
	stopped.Status = vm.StateStopped
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{stopped.ID: stopped})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
	}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "stopped", *out.InstanceStatuses[0].InstanceState.Name)
	assert.Equal(t, "not-applicable", *out.InstanceStatuses[0].InstanceStatus.Status)
	assert.Equal(t, "not-applicable", *out.InstanceStatuses[0].SystemStatus.Status)
}

func TestDescribeInstanceStatus_TerminatedNeverReturned(t *testing.T) {
	owner := "111122223333"
	term := runningVM("i-term", owner)
	term.Status = vm.StateTerminated
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{term.ID: term})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
	}, owner)
	require.NoError(t, err)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_ErrorStateNeverReturned(t *testing.T) {
	owner := "111122223333"
	errVM := runningVM("i-err", owner)
	errVM.Status = vm.StateError
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{errVM.ID: errVM})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
	}, owner)
	require.NoError(t, err)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_InstanceIDFilter(t *testing.T) {
	owner := "111122223333"
	keep := runningVM("i-keep", owner)
	drop := runningVM("i-drop", owner)
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{keep.ID: keep, drop.ID: drop})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		InstanceIds: []*string{aws.String("i-keep")},
	}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "i-keep", *out.InstanceStatuses[0].InstanceId)
}

func TestDescribeInstanceStatus_MalformedInstanceID(t *testing.T) {
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{})
	_, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		InstanceIds: []*string{aws.String("not-an-id")},
	}, utils.GlobalAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestDescribeInstanceStatus_UnknownFilter(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-aaa", owner)
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	_, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("event.code"),
			Values: []*string{aws.String("system-reboot")},
		}},
	}, owner)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeInstanceStatus_StateNameFilter(t *testing.T) {
	owner := "111122223333"
	run := runningVM("i-run", owner)
	stop := runningVM("i-stop", owner)
	stop.Status = vm.StateStopped
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{run.ID: run, stop.ID: stop})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		IncludeAllInstances: aws.Bool(true),
		Filters: []*ec2.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []*string{aws.String("running")},
		}},
	}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "i-run", *out.InstanceStatuses[0].InstanceId)
}

func TestDescribeInstanceStatus_AvailabilityZoneFilter(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-aaa", owner)
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("availability-zone"),
			Values: []*string{aws.String("az-a")},
		}},
	}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)

	out, err = svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("availability-zone"),
			Values: []*string{aws.String("az-b")},
		}},
	}, owner)
	require.NoError(t, err)
	assert.Empty(t, out.InstanceStatuses)
}

func TestDescribeInstanceStatus_TagFilter(t *testing.T) {
	owner := "111122223333"
	tagged := runningVM("i-tag", owner)
	tagged.Instance.Tags = []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("foo")}}
	plain := runningVM("i-plain", owner)
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{tagged.ID: tagged, plain.ID: plain})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:Name"),
			Values: []*string{aws.String("foo")},
		}},
	}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "i-tag", *out.InstanceStatuses[0].InstanceId)
}

func TestDescribeInstanceStatus_QMPUnresponsiveImpaired(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-imp", owner)
	since := time.Now().Add(-90 * time.Second)
	v.Health.QMPConsecutiveFailures = vm.QMPMaxConsecutiveFailures
	v.Health.ImpairedSince = since
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)

	s := out.InstanceStatuses[0]
	assert.Equal(t, "running", *s.InstanceState.Name)
	assert.Equal(t, "impaired", *s.InstanceStatus.Status)
	require.Len(t, s.InstanceStatus.Details, 1)
	assert.Equal(t, "failed", *s.InstanceStatus.Details[0].Status)
	require.NotNil(t, s.InstanceStatus.Details[0].ImpairedSince)
	assert.WithinDuration(t, since, *s.InstanceStatus.Details[0].ImpairedSince, time.Second)
	// SystemStatus is host-level: the node is still reachable.
	assert.Equal(t, "ok", *s.SystemStatus.Status)
}

func TestDescribeInstanceStatus_BelowThresholdStaysOK(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-ok", owner)
	v.Health.QMPConsecutiveFailures = vm.QMPMaxConsecutiveFailures - 1
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "ok", *out.InstanceStatuses[0].InstanceStatus.Status)
	assert.Equal(t, "passed", *out.InstanceStatuses[0].InstanceStatus.Details[0].Status)
}

func TestDescribeInstanceStatus_RecentLaunchInitializing(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-new", owner)
	now := time.Now()
	v.Instance.LaunchTime = &now
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "initializing", *out.InstanceStatuses[0].InstanceStatus.Status)
	assert.Equal(t, "initializing", *out.InstanceStatuses[0].InstanceStatus.Details[0].Status)
}

func TestDescribeInstanceStatus_PastGraceOK(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-old", owner)
	old := time.Now().Add(-5 * time.Minute)
	v.Instance.LaunchTime = &old
	svc := instanceStatusService(t, "az-a", map[string]*vm.VM{v.ID: v})

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "ok", *out.InstanceStatuses[0].InstanceStatus.Status)
}

func TestDescribeInstanceStatus_MemoryPressureSystemImpaired(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-press", owner)
	svc := &InstanceServiceImpl{
		config:      &config.Config{AZ: "az-a"},
		vmMgr:       mgrWith(map[string]*vm.VM{v.ID: v}),
		resourceMgr: &fakeResourceCapacityProvider{memPressure: true},
	}

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)

	s := out.InstanceStatuses[0]
	// Instance itself is healthy; only the host system status is impaired.
	assert.Equal(t, "ok", *s.InstanceStatus.Status)
	assert.Equal(t, "impaired", *s.SystemStatus.Status)
	assert.Equal(t, "failed", *s.SystemStatus.Details[0].Status)
}

func TestDescribeInstanceStatus_NoPressureSystemOK(t *testing.T) {
	owner := "111122223333"
	v := runningVM("i-fine", owner)
	svc := &InstanceServiceImpl{
		config:      &config.Config{AZ: "az-a"},
		vmMgr:       mgrWith(map[string]*vm.VM{v.ID: v}),
		resourceMgr: &fakeResourceCapacityProvider{memPressure: false},
	}

	out, err := svc.DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	assert.Equal(t, "ok", *out.InstanceStatuses[0].SystemStatus.Status)
}

// TestInstanceArchitecture pins the safe-extraction contract: malformed
// InstanceTypeInfo returns "" rather than panicking, and the firmware probe
// surfaces "" as a clear error on the launch path.
func TestInstanceArchitecture(t *testing.T) {
	tests := []struct {
		name string
		it   *ec2.InstanceTypeInfo
		want string
	}{
		{"nil", nil, ""},
		{"nil processor info", &ec2.InstanceTypeInfo{}, ""},
		{"empty supported archs", &ec2.InstanceTypeInfo{ProcessorInfo: &ec2.ProcessorInfo{}}, ""},
		{
			"x86_64",
			&ec2.InstanceTypeInfo{ProcessorInfo: &ec2.ProcessorInfo{SupportedArchitectures: []*string{aws.String("x86_64")}}},
			"x86_64",
		},
		{
			"arm64",
			&ec2.InstanceTypeInfo{ProcessorInfo: &ec2.ProcessorInfo{SupportedArchitectures: []*string{aws.String("arm64")}}},
			"arm64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, instanceArchitecture(tc.it))
		})
	}
}

func TestPrepareRunInstances_PreCreatedENIAttachedSkipsAutoCreate(t *testing.T) {
	eni := &fakeENICreator{
		getENIByID: map[string]*ENIInfo{
			"eni-pre": {
				NetworkInterfaceID: "eni-pre",
				SubnetID:           "subnet-pre",
				VpcID:              "vpc-pre",
				PrivateIpAddress:   "10.0.5.42",
				MacAddress:         "aa:bb:cc:dd:ee:01",
				Status:             "available",
				SecurityGroupIDs:   []string{"sg-pre"},
			},
		},
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)

	_, instances, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			NetworkInterfaceId: aws.String("eni-pre"),
			DeviceIndex:        aws.Int64(0),
		}},
	}, "acc", "")
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, "eni-pre", instances[0].ENIId)
	assert.Equal(t, "aa:bb:cc:dd:ee:01", instances[0].ENIMac)
	assert.Equal(t, 1, eni.attachCalls)
	assert.Equal(t, 0, eni.createCalls, "auto-create must not run when NetworkInterfaceId is specified")
}

func TestPrepareRunInstances_PreCreatedENIInUseRejected(t *testing.T) {
	eni := &fakeENICreator{
		getENIByID: map[string]*ENIInfo{
			"eni-busy": {
				NetworkInterfaceID: "eni-busy",
				Status:             "in-use",
			},
		},
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)

	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			NetworkInterfaceId: aws.String("eni-busy"),
		}},
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidNetworkInterfaceInUse, err.Error())
	assert.Equal(t, 0, eni.attachCalls, "in-use ENI must not be attached")
	assert.Equal(t, 0, eni.createCalls, "auto-create must not run when NetworkInterfaceId is specified")
}

func TestPrepareRunInstances_PreCreatedENILookupErrorSurfaced(t *testing.T) {
	eni := &fakeENICreator{
		getENIErr: errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound),
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)

	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			NetworkInterfaceId: aws.String("eni-ghost"),
		}},
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidNetworkInterfaceIDNotFound, err.Error())
	assert.Equal(t, 0, eni.attachCalls)
	assert.Equal(t, 0, eni.createCalls)
}

func TestPrepareRunInstances_PreCreatedENIAttachErrorSurfaced(t *testing.T) {
	eni := &fakeENICreator{
		getENIByID: map[string]*ENIInfo{
			"eni-pre": {
				NetworkInterfaceID: "eni-pre",
				Status:             "available",
				PrivateIpAddress:   "10.0.5.42",
				MacAddress:         "aa:bb:cc:dd:ee:01",
				SubnetID:           "subnet-pre",
				VpcID:              "vpc-pre",
			},
		},
		attachErr: errors.New(awserrors.ErrorServerInternal),
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)

	_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			NetworkInterfaceId: aws.String("eni-pre"),
		}},
	}, "acc", "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Equal(t, 1, eni.attachCalls)
	assert.Equal(t, 0, eni.createCalls)
}
