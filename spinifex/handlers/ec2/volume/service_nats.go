package handlers_ec2_volume

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var _ VolumeService = (*NATSVolumeService)(nil)

// NATSVolumeService handles volume operations via NATS messaging
type NATSVolumeService struct {
	natsConn *nats.Conn
}

// NewNATSVolumeService creates a new NATS-based volume service
func NewNATSVolumeService(conn *nats.Conn) VolumeService {
	return &NATSVolumeService{natsConn: conn}
}

func (s *NATSVolumeService) CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	return utils.NATSRequestCtx[ec2.Volume](ctx, s.natsConn, "ec2.CreateVolume", input, 30*time.Second, accountID)
}

func (s *NATSVolumeService) DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, accountID string) (*ec2.DescribeVolumesOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeVolumesOutput](ctx, s.natsConn, "ec2.DescribeVolumes", input, 30*time.Second, accountID)
}

func (s *NATSVolumeService) ModifyVolume(ctx context.Context, input *ec2.ModifyVolumeInput, accountID string) (*ec2.ModifyVolumeOutput, error) {
	return utils.NATSRequestCtx[ec2.ModifyVolumeOutput](ctx, s.natsConn, "ec2.ModifyVolume", input, 30*time.Second, accountID)
}

func (s *NATSVolumeService) DescribeVolumeStatus(ctx context.Context, input *ec2.DescribeVolumeStatusInput, accountID string) (*ec2.DescribeVolumeStatusOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeVolumeStatusOutput](ctx, s.natsConn, "ec2.DescribeVolumeStatus", input, 30*time.Second, accountID)
}

func (s *NATSVolumeService) DescribeVolumesModifications(ctx context.Context, input *ec2.DescribeVolumesModificationsInput, accountID string) (*ec2.DescribeVolumesModificationsOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeVolumesModificationsOutput](ctx, s.natsConn, "ec2.DescribeVolumesModifications", input, 30*time.Second, accountID)
}

func (s *NATSVolumeService) DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error) {
	return utils.NATSRequestCtx[ec2.DeleteVolumeOutput](ctx, s.natsConn, "ec2.DeleteVolume", input, 30*time.Second, accountID)
}
