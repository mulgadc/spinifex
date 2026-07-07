package daemon

import (
	"context"

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

func (a *daemonENICreator) GetDefaultSubnet(_ context.Context, accountID string) (*handlers_ec2_instance.SubnetInfo, error) {
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

func (a *daemonENICreator) GetSubnet(_ context.Context, accountID, subnetID string) (*handlers_ec2_instance.SubnetInfo, error) {
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

func (a *daemonENICreator) GetENI(_ context.Context, accountID, eniID string) (*handlers_ec2_instance.ENIInfo, error) {
	rec, err := a.d.vpcService.GetENIRecord(accountID, eniID)
	if err != nil {
		return nil, err
	}
	return &handlers_ec2_instance.ENIInfo{
		NetworkInterfaceID: rec.NetworkInterfaceId,
		SubnetID:           rec.SubnetId,
		VpcID:              rec.VpcId,
		PrivateIpAddress:   rec.PrivateIpAddress,
		MacAddress:         rec.MacAddress,
		Status:             rec.Status,
		SecurityGroupIDs:   rec.SecurityGroupIds,
	}, nil
}

func (a *daemonENICreator) CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error) {
	return a.d.vpcService.CreateNetworkInterface(ctx, input, accountID)
}

func (a *daemonENICreator) AttachENI(_ context.Context, accountID, eniID, instanceID string, deviceIndex int64) (string, error) {
	return a.d.vpcService.AttachENI(accountID, eniID, instanceID, deviceIndex)
}

func (a *daemonENICreator) DetachENI(ctx context.Context, accountID, eniID string) error {
	return a.d.vpcService.DetachENI(ctx, accountID, eniID)
}

func (a *daemonENICreator) UpdateENIPublicIP(_ context.Context, accountID, eniID, publicIP, poolName string) error {
	return a.d.vpcService.UpdateENIPublicIP(accountID, eniID, publicIP, poolName)
}
