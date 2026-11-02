package daemon

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

func publicIPChanged(t *testing.T, d *Daemon, instanceID, publicIP string) {
	t.Helper()
	data, err := json.Marshal(ec2eip.ENIPublicIPChanged{InstanceID: instanceID, ENIID: "eni-1", PublicIP: publicIP, PoolName: "p"})
	require.NoError(t, err)
	require.Equal(t, outcomeSuccess, d.handleENIPublicIPChanged(&nats.Msg{Data: data}))
}

func describedPublicIP(t *testing.T, d *Daemon, instanceID string) string {
	t.Helper()
	v, ok := d.vmMgr.Get(instanceID)
	require.True(t, ok)
	inst, _ := ec2instance.ProjectInstance(v, ec2instance.InstanceProjection{IncludeRuntimeNetwork: true})
	return aws.StringValue(inst.PublicIpAddress)
}

// Associating an EIP releases the launch address, so after a disassociate the
// instance must not go back to reporting it: that address is no longer ours.
func TestENIPublicIPChanged_DisassociateDoesNotRevealReleasedLaunchAddress(t *testing.T) {
	d := createDaemonWithJetStream(t)
	d.vmMgr.Insert(&vm.VM{
		ID: "i-eip", Status: vm.StateRunning, PublicIP: "198.51.100.7",
		Instance: &ec2.Instance{InstanceId: aws.String("i-eip"), PublicIpAddress: aws.String("198.51.100.7")},
	})

	publicIPChanged(t, d, "i-eip", "203.0.113.9")
	assert.Equal(t, "203.0.113.9", describedPublicIP(t, d, "i-eip"))

	publicIPChanged(t, d, "i-eip", "")
	assert.Empty(t, describedPublicIP(t, d, "i-eip"))
}
