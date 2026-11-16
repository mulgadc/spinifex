package instance

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const describeENIOwner = "111122223333"

// describeENIVM is a running instance whose stored interfaces are the
// launch-time copy: the primary only.
func describeENIVM() *vm.VM {
	return &vm.VM{
		ID:        "i-hotplug",
		Status:    vm.StateRunning,
		AccountID: describeENIOwner,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-hotplug"),
			OwnerId:       aws.String(describeENIOwner),
		},
		Instance: &ec2.Instance{
			InstanceId:     aws.String("i-hotplug"),
			SecurityGroups: []*ec2.GroupIdentifier{{GroupId: aws.String("sg-1"), GroupName: aws.String("default")}},
			NetworkInterfaces: []*ec2.InstanceNetworkInterface{{
				NetworkInterfaceId: aws.String("eni-primary"),
				Attachment:         &ec2.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int64(0)},
			}},
		},
	}
}

func primaryENI() ENIInfo {
	return ENIInfo{
		NetworkInterfaceID: "eni-primary", PrivateIpAddress: "10.0.0.10", Status: "in-use",
		SecurityGroupIDs: []string{"sg-1"}, AttachmentID: "eni-attach-p", DeviceIndex: 0, DeleteOnTermination: true,
	}
}

func secondaryENI() ENIInfo {
	return ENIInfo{
		NetworkInterfaceID: "eni-second", PrivateIpAddress: "10.0.0.11", Status: "in-use",
		SecurityGroupIDs: []string{"sg-1"}, AttachmentID: "eni-attach-s", DeviceIndex: 1,
	}
}

func describedInterfaces(t *testing.T, out *ec2.DescribeInstancesOutput) []*ec2.InstanceNetworkInterface {
	t.Helper()
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)
	return out.Reservations[0].Instances[0].NetworkInterfaces
}

func interfaceIDs(nics []*ec2.InstanceNetworkInterface) []string {
	ids := make([]string, 0, len(nics))
	for _, nic := range nics {
		ids = append(ids, aws.StringValue(nic.NetworkInterfaceId))
	}
	return ids
}

// An interface attached after launch must appear in DescribeInstances, and go
// again once detached, on both the running and the stopped path.
func TestDescribeInstances_InterfacesFollowAttachAndDetach(t *testing.T) {
	v := describeENIVM()
	enis := &fakeENICreator{instanceENIs: map[string][]ENIInfo{
		v.ID:         {secondaryENI(), primaryENI()},
		"i-neighbor": {{NetworkInterfaceID: "eni-neighbor", DeviceIndex: 0}},
	}}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v}), config: &config.Config{}, eniCreator: enis}
	ctx := context.Background()

	out, err := svc.DescribeInstances(ctx, &ec2.DescribeInstancesInput{}, describeENIOwner)
	require.NoError(t, err)
	nics := describedInterfaces(t, out)
	require.Equal(t, []string{"eni-primary", "eni-second"}, interfaceIDs(nics), "primary first, then the hot-plugged interface")
	second := nics[1]
	assert.Equal(t, "eni-attach-s", aws.StringValue(second.Attachment.AttachmentId))
	assert.Equal(t, int64(1), aws.Int64Value(second.Attachment.DeviceIndex))
	assert.Equal(t, ec2.AttachmentStatusAttached, aws.StringValue(second.Attachment.Status))
	assert.False(t, aws.BoolValue(second.Attachment.DeleteOnTermination))
	assert.Equal(t, "10.0.0.11", aws.StringValue(second.PrivateIpAddress))
	assert.Equal(t, "default", aws.StringValue(second.Groups[0].GroupName))
	assert.True(t, aws.BoolValue(second.SourceDestCheck))

	byIP := &ec2.DescribeInstancesInput{Filters: []*ec2.Filter{{
		Name: aws.String("network-interface.private-ip-address"), Values: []*string{aws.String("10.0.0.11")},
	}}}
	out, err = svc.DescribeInstances(ctx, byIP, describeENIOwner)
	require.NoError(t, err)
	assert.Len(t, out.Reservations, 1, "the hot-plugged interface's address matches the filter")

	stopped := describeENIVM()
	stopped.Status = vm.StateStopped
	listFn := func() ([]*vm.VM, error) { return []*vm.VM{stopped}, nil }
	out, err = svc.describeInstancesFromKV(ctx, &ec2.DescribeInstancesInput{}, describeENIOwner, listFn, 80, "stopped", "DescribeStoppedInstances", false, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"eni-primary", "eni-second"}, interfaceIDs(describedInterfaces(t, out)))

	enis.instanceENIs[v.ID] = []ENIInfo{primaryENI()}
	out, err = svc.DescribeInstances(ctx, &ec2.DescribeInstancesInput{}, describeENIOwner)
	require.NoError(t, err)
	assert.Equal(t, []string{"eni-primary"}, interfaceIDs(describedInterfaces(t, out)), "a detached interface leaves the instance")
}

// A describe that cannot read the interface records fails rather than
// answering with interfaces that may be stale.
func TestDescribeInstances_InterfaceListFailureIsInternal(t *testing.T) {
	v := describeENIVM()
	svc := &InstanceServiceImpl{
		vmMgr:      mgrWith(map[string]*vm.VM{v.ID: v}),
		config:     &config.Config{},
		eniCreator: &fakeENICreator{listENIsErr: errors.New("kv down")},
	}

	_, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, describeENIOwner)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
