package daemon

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
)

// daemonENICreator adapts the daemon's VPCServiceImpl to the
// handlers_ec2_instance.ENICreator interface so RunInstances can stay free of
// the wider VPC service surface. Returned subnet records are translated to
// handlers_ec2_instance.SubnetInfo to avoid a cyclic instance↔vpc import.
type daemonENICreator struct {
	d *Daemon
}

var _ handlers_ec2_instance.ENICreator = (*daemonENICreator)(nil)

func (a *daemonENICreator) GetDefaultSubnet(accountID string) (*handlers_ec2_instance.SubnetInfo, error) {
	rec, err := a.d.vpcService.GetDefaultSubnet(accountID)
	if err != nil {
		return nil, err
	}
	return &handlers_ec2_instance.SubnetInfo{
		SubnetID:            rec.SubnetId,
		VpcID:               rec.VpcId,
		MapPublicIpOnLaunch: rec.MapPublicIpOnLaunch,
	}, nil
}

func (a *daemonENICreator) GetSubnet(accountID, subnetID string) (*handlers_ec2_instance.SubnetInfo, error) {
	rec, err := a.d.vpcService.GetSubnet(accountID, subnetID)
	if err != nil {
		return nil, err
	}
	return &handlers_ec2_instance.SubnetInfo{
		SubnetID:            rec.SubnetId,
		VpcID:               rec.VpcId,
		MapPublicIpOnLaunch: rec.MapPublicIpOnLaunch,
	}, nil
}

func (a *daemonENICreator) CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error) {
	return a.d.vpcService.CreateNetworkInterface(input, accountID)
}

func (a *daemonENICreator) AttachENI(accountID, eniID, instanceID string, deviceIndex int64) (string, error) {
	return a.d.vpcService.AttachENI(accountID, eniID, instanceID, deviceIndex)
}

func (a *daemonENICreator) DetachENI(accountID, eniID string) error {
	return a.d.vpcService.DetachENI(accountID, eniID)
}

func (a *daemonENICreator) UpdateENIPublicIP(accountID, eniID, publicIP, poolName string) error {
	return a.d.vpcService.UpdateENIPublicIP(accountID, eniID, publicIP, poolName)
}
