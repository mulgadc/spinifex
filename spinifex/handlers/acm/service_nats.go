package handlers_acm

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// NATSACMService forwards ACM operations to the daemon over NATS request/response.
type NATSACMService struct {
	natsConn *nats.Conn
}

var _ ACMService = (*NATSACMService)(nil)

// NewNATSACMService creates a NATS-backed ACM service client.
func NewNATSACMService(conn *nats.Conn) ACMService {
	return &NATSACMService{natsConn: conn}
}

func (s *NATSACMService) ImportCertificate(ctx context.Context, input *acm.ImportCertificateInput, accountID string) (*acm.ImportCertificateOutput, error) {
	return utils.NATSRequest[acm.ImportCertificateOutput](ctx, s.natsConn, "acm.ImportCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) DescribeCertificate(ctx context.Context, input *acm.DescribeCertificateInput, accountID string) (*acm.DescribeCertificateOutput, error) {
	return utils.NATSRequest[acm.DescribeCertificateOutput](ctx, s.natsConn, "acm.DescribeCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) ListCertificates(ctx context.Context, input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error) {
	return utils.NATSRequest[acm.ListCertificatesOutput](ctx, s.natsConn, "acm.ListCertificates", input, defaultTimeout, accountID)
}

func (s *NATSACMService) DeleteCertificate(ctx context.Context, input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error) {
	return utils.NATSRequest[acm.DeleteCertificateOutput](ctx, s.natsConn, "acm.DeleteCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) ListTagsForCertificate(ctx context.Context, input *acm.ListTagsForCertificateInput, accountID string) (*acm.ListTagsForCertificateOutput, error) {
	return utils.NATSRequest[acm.ListTagsForCertificateOutput](ctx, s.natsConn, "acm.ListTagsForCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) AddTagsToCertificate(ctx context.Context, input *acm.AddTagsToCertificateInput, accountID string) (*acm.AddTagsToCertificateOutput, error) {
	return utils.NATSRequest[acm.AddTagsToCertificateOutput](ctx, s.natsConn, "acm.AddTagsToCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) RemoveTagsFromCertificate(ctx context.Context, input *acm.RemoveTagsFromCertificateInput, accountID string) (*acm.RemoveTagsFromCertificateOutput, error) {
	return utils.NATSRequest[acm.RemoveTagsFromCertificateOutput](ctx, s.natsConn, "acm.RemoveTagsFromCertificate", input, defaultTimeout, accountID)
}
