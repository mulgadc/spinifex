package handlers_ec2_launchtemplate

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSLaunchTemplateService is the gateway-side client that forwards launch
// template operations to the daemon-hosted service impl over NATS.
type NATSLaunchTemplateService struct {
	natsConn *nats.Conn
}

var _ LaunchTemplateService = (*NATSLaunchTemplateService)(nil)

// NewNATSLaunchTemplateService creates a new NATS-based launch template service.
func NewNATSLaunchTemplateService(conn *nats.Conn) LaunchTemplateService {
	return &NATSLaunchTemplateService{natsConn: conn}
}

func (s *NATSLaunchTemplateService) CreateLaunchTemplate(ctx context.Context, input *ec2.CreateLaunchTemplateInput, accountID string) (*ec2.CreateLaunchTemplateOutput, error) {
	return utils.NATSRequest[ec2.CreateLaunchTemplateOutput](ctx, s.natsConn, "ec2.CreateLaunchTemplate", input, 30*time.Second, accountID)
}

func (s *NATSLaunchTemplateService) CreateLaunchTemplateVersion(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, accountID string) (*ec2.CreateLaunchTemplateVersionOutput, error) {
	return utils.NATSRequest[ec2.CreateLaunchTemplateVersionOutput](ctx, s.natsConn, "ec2.CreateLaunchTemplateVersion", input, 30*time.Second, accountID)
}

func (s *NATSLaunchTemplateService) DeleteLaunchTemplate(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, accountID string) (*ec2.DeleteLaunchTemplateOutput, error) {
	return utils.NATSRequest[ec2.DeleteLaunchTemplateOutput](ctx, s.natsConn, "ec2.DeleteLaunchTemplate", input, 30*time.Second, accountID)
}

func (s *NATSLaunchTemplateService) DeleteLaunchTemplateVersions(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, accountID string) (*ec2.DeleteLaunchTemplateVersionsOutput, error) {
	return utils.NATSRequest[ec2.DeleteLaunchTemplateVersionsOutput](ctx, s.natsConn, "ec2.DeleteLaunchTemplateVersions", input, 30*time.Second, accountID)
}

func (s *NATSLaunchTemplateService) ModifyLaunchTemplate(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, accountID string) (*ec2.ModifyLaunchTemplateOutput, error) {
	return utils.NATSRequest[ec2.ModifyLaunchTemplateOutput](ctx, s.natsConn, "ec2.ModifyLaunchTemplate", input, 30*time.Second, accountID)
}

func (s *NATSLaunchTemplateService) DescribeLaunchTemplates(ctx context.Context, input *ec2.DescribeLaunchTemplatesInput, accountID string) (*ec2.DescribeLaunchTemplatesOutput, error) {
	return utils.NATSRequest[ec2.DescribeLaunchTemplatesOutput](ctx, s.natsConn, "ec2.DescribeLaunchTemplates", input, 30*time.Second, accountID)
}

func (s *NATSLaunchTemplateService) DescribeLaunchTemplateVersions(ctx context.Context, input *ec2.DescribeLaunchTemplateVersionsInput, accountID string) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	return utils.NATSRequest[ec2.DescribeLaunchTemplateVersionsOutput](ctx, s.natsConn, "ec2.DescribeLaunchTemplateVersions", input, 30*time.Second, accountID)
}
