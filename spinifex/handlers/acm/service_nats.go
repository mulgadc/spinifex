package handlers_acm

import (
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

func (s *NATSACMService) ImportCertificate(input *acm.ImportCertificateInput, accountID string) (*acm.ImportCertificateOutput, error) {
	return utils.NATSRequest[acm.ImportCertificateOutput](s.natsConn, "acm.ImportCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) DescribeCertificate(input *acm.DescribeCertificateInput, accountID string) (*acm.DescribeCertificateOutput, error) {
	return utils.NATSRequest[acm.DescribeCertificateOutput](s.natsConn, "acm.DescribeCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) ListCertificates(input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error) {
	return utils.NATSRequest[acm.ListCertificatesOutput](s.natsConn, "acm.ListCertificates", input, defaultTimeout, accountID)
}

func (s *NATSACMService) DeleteCertificate(input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error) {
	return utils.NATSRequest[acm.DeleteCertificateOutput](s.natsConn, "acm.DeleteCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) ListTagsForCertificate(input *acm.ListTagsForCertificateInput, accountID string) (*acm.ListTagsForCertificateOutput, error) {
	return utils.NATSRequest[acm.ListTagsForCertificateOutput](s.natsConn, "acm.ListTagsForCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) AddTagsToCertificate(input *acm.AddTagsToCertificateInput, accountID string) (*acm.AddTagsToCertificateOutput, error) {
	return utils.NATSRequest[acm.AddTagsToCertificateOutput](s.natsConn, "acm.AddTagsToCertificate", input, defaultTimeout, accountID)
}

func (s *NATSACMService) RemoveTagsFromCertificate(input *acm.RemoveTagsFromCertificateInput, accountID string) (*acm.RemoveTagsFromCertificateOutput, error) {
	return utils.NATSRequest[acm.RemoveTagsFromCertificateOutput](s.natsConn, "acm.RemoveTagsFromCertificate", input, defaultTimeout, accountID)
}
