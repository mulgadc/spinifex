//test:in-package — extends projection_test.go's in-package fullVM fixture and
// projects through the unexported field set it builds.

package handlers_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stampedVM is a launched instance: the auto-assigned address is written onto
// the stored ec2.Instance at launch and stays there for the instance's life.
func stampedVM(status vm.InstanceState, runtimePublicIP string) *vm.VM {
	v := fullVM(status)
	v.PublicIP = runtimePublicIP
	v.Instance.PublicIpAddress = aws.String("203.0.113.7")
	v.Instance.PublicDnsName = aws.String("ec2-203-0-113-7.example.com")
	v.Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{{
		NetworkInterfaceId: aws.String("eni-1"),
		Attachment:         &ec2.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int64(0)},
		Association: &ec2.InstanceNetworkInterfaceAssociation{
			PublicIp: aws.String("203.0.113.7"),
		},
	}}
	return v
}

// AWS releases an auto-assigned address when an instance stops. Ours kept
// reporting it, because the launch stamp lives on the stored instance and the
// stopped path only declined to add an address — it never removed one.
func TestStoppedInstanceReportsNoPublicAddress(t *testing.T) {
	v := stampedVM(vm.StateStopped, "")

	got, _ := ProjectInstance(v, InstanceProjection{
		Region: "us-west-1", IncludeRuntimeNetwork: false,
		FallbackStateCode: 80, FallbackStateName: "stopped",
	})

	assert.Nil(t, got.PublicIpAddress, "a stopped instance still advertises a released address")
	assert.Nil(t, got.PublicDnsName)
	require.Len(t, got.NetworkInterfaces, 1)
	assert.Nil(t, got.NetworkInterfaces[0].Association,
		"the primary NIC still advertises the released association")
}

// Clearing the projection must not reach into the stored instance, or the
// address is gone for good and a restart cannot report it again.
func TestClearingTheStoppedAddressLeavesTheStoredInstanceIntact(t *testing.T) {
	v := stampedVM(vm.StateStopped, "")

	_, _ = ProjectInstance(v, InstanceProjection{
		Region: "us-west-1", IncludeRuntimeNetwork: false,
		FallbackStateCode: 80, FallbackStateName: "stopped",
	})

	assert.Equal(t, "203.0.113.7", aws.StringValue(v.Instance.PublicIpAddress))
	require.Len(t, v.Instance.NetworkInterfaces, 1)
	require.NotNil(t, v.Instance.NetworkInterfaces[0].Association)
}

// Associating an EIP replaces what the instance is reachable on. The old
// projection only filled a nil field, and the launch stamp is never nil, so the
// EIP could not be reported however correctly it was recorded.
func TestTheRuntimeAddressWinsOverTheLaunchStamp(t *testing.T) {
	v := stampedVM(vm.StateRunning, "198.51.100.42")

	got, _ := ProjectInstance(v, InstanceProjection{
		Region: "us-west-1", IncludeRuntimeNetwork: true,
		FallbackStateCode: 16, FallbackStateName: "running",
	})

	assert.Equal(t, "198.51.100.42", aws.StringValue(got.PublicIpAddress))
	require.Len(t, got.NetworkInterfaces, 1)
	require.NotNil(t, got.NetworkInterfaces[0].Association)
	assert.Equal(t, "198.51.100.42", aws.StringValue(got.NetworkInterfaces[0].Association.PublicIp))
}

// With no EIP the auto-assigned address is still the answer, so the stamp must
// survive a running projection.
func TestTheLaunchStampStandsWhenThereIsNoRuntimeAddress(t *testing.T) {
	v := stampedVM(vm.StateRunning, "")

	got, _ := ProjectInstance(v, InstanceProjection{
		Region: "us-west-1", IncludeRuntimeNetwork: true,
		FallbackStateCode: 16, FallbackStateName: "running",
	})

	assert.Equal(t, "203.0.113.7", aws.StringValue(got.PublicIpAddress))
}
