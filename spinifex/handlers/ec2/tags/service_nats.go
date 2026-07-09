package handlers_ec2_tags

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var _ TagsService = (*NATSTagsService)(nil)

// NATSTagsService handles tag operations via NATS messaging
type NATSTagsService struct {
	natsConn *nats.Conn
}

// NewNATSTagsService creates a new NATS-based tags service
func NewNATSTagsService(conn *nats.Conn) TagsService {
	return &NATSTagsService{natsConn: conn}
}

func (s *NATSTagsService) CreateTags(ctx context.Context, input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error) {
	return utils.NATSRequest[ec2.CreateTagsOutput](ctx, s.natsConn, "ec2.CreateTags", input, 30*time.Second, accountID)
}

func (s *NATSTagsService) DescribeTags(ctx context.Context, input *ec2.DescribeTagsInput, accountID string) (*ec2.DescribeTagsOutput, error) {
	return utils.NATSRequest[ec2.DescribeTagsOutput](ctx, s.natsConn, "ec2.DescribeTags", input, 30*time.Second, accountID)
}

func (s *NATSTagsService) DeleteTags(ctx context.Context, input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error) {
	return utils.NATSRequest[ec2.DeleteTagsOutput](ctx, s.natsConn, "ec2.DeleteTags", input, 30*time.Second, accountID)
}
