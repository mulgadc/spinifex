package handlers_ec2_key

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var _ KeyService = (*NATSKeyService)(nil)

// NATSKeyService handles key operations via NATS messaging
type NATSKeyService struct {
	natsConn *nats.Conn
}

// NewNATSKeyService creates a new NATS-based key service
func NewNATSKeyService(conn *nats.Conn) KeyService {
	return &NATSKeyService{natsConn: conn}
}

func (s *NATSKeyService) CreateKeyPair(ctx context.Context, input *ec2.CreateKeyPairInput, accountID string) (*ec2.CreateKeyPairOutput, error) {
	return utils.NATSRequestCtx[ec2.CreateKeyPairOutput](ctx, s.natsConn, "ec2.CreateKeyPair", input, 30*time.Second, accountID)
}

func (s *NATSKeyService) DeleteKeyPair(ctx context.Context, input *ec2.DeleteKeyPairInput, accountID string) (*ec2.DeleteKeyPairOutput, error) {
	return utils.NATSRequestCtx[ec2.DeleteKeyPairOutput](ctx, s.natsConn, "ec2.DeleteKeyPair", input, 30*time.Second, accountID)
}

func (s *NATSKeyService) DescribeKeyPairs(ctx context.Context, input *ec2.DescribeKeyPairsInput, accountID string) (*ec2.DescribeKeyPairsOutput, error) {
	return utils.NATSRequestCtx[ec2.DescribeKeyPairsOutput](ctx, s.natsConn, "ec2.DescribeKeyPairs", input, 30*time.Second, accountID)
}

func (s *NATSKeyService) ImportKeyPair(ctx context.Context, input *ec2.ImportKeyPairInput, accountID string) (*ec2.ImportKeyPairOutput, error) {
	return utils.NATSRequestCtx[ec2.ImportKeyPairOutput](ctx, s.natsConn, "ec2.ImportKeyPair", input, 30*time.Second, accountID)
}
