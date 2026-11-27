package instance

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
)

const (
	namedAccount = "111122223333"
	namedOther   = "444455556666"
	namedID      = "i-0123456789abcdef0"
)

func namedNIC(eni, vpc, subnet string, index int64) *ec2.InstanceNetworkInterface {
	return &ec2.InstanceNetworkInterface{
		NetworkInterfaceId: aws.String(eni), VpcId: aws.String(vpc), SubnetId: aws.String(subnet),
		Attachment: &ec2.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int64(index)},
	}
}

// namedVM is a running instance whose primary interface is eni-primary in
// vpc-a, as launch records it.
func namedVM() *vm.VM {
	return &vm.VM{
		ID: namedID, AccountID: namedAccount, Status: vm.StateRunning, ENIId: "eni-primary",
		Instance: &ec2.Instance{
			InstanceId: aws.String(namedID), VpcId: aws.String("vpc-a"), SubnetId: aws.String("subnet-a"),
			NetworkInterfaces: []*ec2.InstanceNetworkInterface{namedNIC("eni-primary", "vpc-a", "subnet-a", 0)},
		},
	}
}

func TestProjectNamedInstance(t *testing.T) {
	running := &projection.NamedInstance{
		InstanceID: namedID, AccountID: namedAccount, State: "running",
		PrimaryInterfaceID: "eni-primary", VpcID: "vpc-a", SubnetID: "subnet-a",
	}
	now := time.Now()
	for name, tc := range map[string]struct {
		caller string
		edit   func(*vm.VM)
		want   func(*projection.NamedInstance) *projection.NamedInstance
	}{
		"running": {namedAccount, func(*vm.VM) {}, func(n *projection.NamedInstance) *projection.NamedInstance { return n }},
		"stopped keeps its placement": {namedAccount, func(v *vm.VM) { v.Status = vm.StateStopped },
			func(n *projection.NamedInstance) *projection.NamedInstance { n.State = "stopped"; return n }},
		"terminate decided": {namedAccount, func(v *vm.VM) { v.DeletionTimestamp = &now },
			func(n *projection.NamedInstance) *projection.NamedInstance { n.Deleting = true; return n }},
		"other account":            {namedOther, func(*vm.VM) {}, nil},
		"managed, customer caller": {namedAccount, func(v *vm.VM) { v.ManagedBy = "elbv2" }, nil},
		"unowned, customer caller": {namedAccount, func(v *vm.VM) { v.AccountID = "" }, nil},
		"managed, global caller": {awsidentifiers.GlobalAccountID, func(v *vm.VM) { v.ManagedBy, v.AccountID = "elbv2", awsidentifiers.GlobalAccountID },
			func(n *projection.NamedInstance) *projection.NamedInstance { n.AccountID = awsidentifiers.GlobalAccountID; return n }},
		"primary in another VPC, matching secondary": {namedAccount, func(v *vm.VM) {
			v.Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{
				namedNIC("eni-second", "vpc-a", "subnet-a", 1), namedNIC("eni-primary", "vpc-b", "subnet-b", 0),
			}
		}, func(n *projection.NamedInstance) *projection.NamedInstance { n.VpcID, n.SubnetID = "vpc-b", "subnet-b"; return n }},
		"device 0 is not the recorded primary": {namedAccount, func(v *vm.VM) {
			v.Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{namedNIC("eni-other", "vpc-a", "subnet-a", 0)}
		}, func(n *projection.NamedInstance) *projection.NamedInstance {
			n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
			return n
		}},
		"no recorded primary": {namedAccount, func(v *vm.VM) { v.ENIId = "" },
			func(n *projection.NamedInstance) *projection.NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
		"no interfaces": {namedAccount, func(v *vm.VM) { v.Instance.NetworkInterfaces = nil },
			func(n *projection.NamedInstance) *projection.NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
		"no attachment index": {namedAccount, func(v *vm.VM) { v.Instance.NetworkInterfaces[0].Attachment.DeviceIndex = nil },
			func(n *projection.NamedInstance) *projection.NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
		"no instance projection": {namedAccount, func(v *vm.VM) { v.Instance = nil },
			func(n *projection.NamedInstance) *projection.NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
	} {
		t.Run(name, func(t *testing.T) {
			v := namedVM()
			tc.edit(v)
			got := ProjectNamedInstance(tc.caller, namedID, v)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			want := *running
			assert.Equal(t, tc.want(&want), got)
			assert.Equal(t, v.Status == vm.StateRunning, got.Running(), "the predicate follows the recorded state")
		})
	}
}
