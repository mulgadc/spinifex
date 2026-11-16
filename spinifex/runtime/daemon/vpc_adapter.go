package daemon

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
)

// daemonENICreator adapts the daemon's VPCServiceImpl to the
// ec2instance.ENICreator interface so RunInstances can stay free of
// the wider VPC service surface. Returned subnet records are translated to
// ec2instance.SubnetInfo to avoid a cyclic instance↔vpc import.
type daemonENICreator struct {
	d *Daemon
}

var _ ec2instance.ENICreator = (*daemonENICreator)(nil)

func (a *daemonENICreator) GetDefaultSubnet(_ context.Context, accountID string) (*ec2instance.SubnetInfo, error) {
	rec, err := a.d.vpcService.GetDefaultSubnet(accountID)
	if err != nil {
		return nil, err
	}
	return &ec2instance.SubnetInfo{
		SubnetID:            rec.SubnetId,
		VpcID:               rec.VpcId,
		MapPublicIpOnLaunch: rec.MapPublicIpOnLaunch,
	}, nil
}

func (a *daemonENICreator) GetSubnet(_ context.Context, accountID, subnetID string) (*ec2instance.SubnetInfo, error) {
	rec, err := a.d.vpcService.GetSubnet(accountID, subnetID)
	if err != nil {
		return nil, err
	}
	return &ec2instance.SubnetInfo{
		SubnetID:            rec.SubnetId,
		VpcID:               rec.VpcId,
		MapPublicIpOnLaunch: rec.MapPublicIpOnLaunch,
	}, nil
}

func (a *daemonENICreator) GetENI(_ context.Context, accountID, eniID string) (*ec2instance.ENIInfo, error) {
	rec, err := a.d.vpcService.GetENIRecord(accountID, eniID)
	if err != nil {
		return nil, err
	}
	info := eniInfoFromRecord(rec)
	return &info, nil
}

func eniInfoFromRecord(rec ec2vpc.ENIRecord) ec2instance.ENIInfo {
	return ec2instance.ENIInfo{
		NetworkInterfaceID: rec.NetworkInterfaceId,
		SubnetID:           rec.SubnetId,
		VpcID:              rec.VpcId,
		PrivateIpAddress:   rec.PrivateIpAddress,
		MacAddress:         rec.MacAddress,
		Description:        rec.Description,
		Status:             rec.Status,
		SecurityGroupIDs:   rec.SecurityGroupIds,
		PublicIpAddress:    rec.PublicIpAddress,
		PublicIpPool:       rec.PublicIpPool,
		DeleteOnTermination: rec.DeleteOnTermination == nil ||
			*rec.DeleteOnTermination,
		InstanceID:       rec.InstanceId,
		AttachmentID:     rec.AttachmentId,
		DeviceIndex:      rec.DeviceIndex,
		AttachmentStatus: rec.AttachmentStatus,
		AttachTime:       rec.AttachTime,
	}
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

func (a *daemonENICreator) ENIHasEIP(ctx context.Context, accountID, eniID string) (bool, error) {
	return a.d.vpcService.ENIHasEIP(ctx, accountID, eniID)
}

func (a *daemonENICreator) ListInstanceENIs(_ context.Context, accountID, instanceID string) ([]ec2instance.ENIInfo, error) {
	return eniInfosFromRecords(a.d.vpcService.ListInstanceENIs(accountID, instanceID))
}

func (a *daemonENICreator) InstanceAttachedENIs(ctx context.Context, accountID, instanceID string, candidates []string) ([]ec2instance.ENIInfo, error) {
	return eniInfosFromRecords(a.d.vpcService.InstanceAttachedENIs(ctx, accountID, instanceID, candidates))
}

func eniInfosFromRecords(records []ec2vpc.ENIRecord, err error) ([]ec2instance.ENIInfo, error) {
	if err != nil {
		return nil, err
	}
	out := make([]ec2instance.ENIInfo, 0, len(records))
	for _, rec := range records {
		out = append(out, eniInfoFromRecord(rec))
	}
	return out, nil
}
