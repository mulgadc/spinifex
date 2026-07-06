package handlers_ec2_volume

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// VolumeService defines the interface for EBS volume operations
type VolumeService interface {
	CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error)
	DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, accountID string) (*ec2.DescribeVolumesOutput, error)
	ModifyVolume(ctx context.Context, input *ec2.ModifyVolumeInput, accountID string) (*ec2.ModifyVolumeOutput, error)
	DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error)
	DescribeVolumeStatus(ctx context.Context, input *ec2.DescribeVolumeStatusInput, accountID string) (*ec2.DescribeVolumeStatusOutput, error)
	DescribeVolumesModifications(ctx context.Context, input *ec2.DescribeVolumesModificationsInput, accountID string) (*ec2.DescribeVolumesModificationsOutput, error)
}
