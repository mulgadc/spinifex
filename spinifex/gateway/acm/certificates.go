package gateway_acm

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	"github.com/nats-io/nats.go"
)

// ImportCertificate — CertificateManager.ImportCertificate
func ImportCertificate(natsConn *nats.Conn, accountID string, body []byte) (*acm.ImportCertificateOutput, error) {
	input := new(acm.ImportCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).ImportCertificate(input, accountID)
}

// DescribeCertificate — CertificateManager.DescribeCertificate
func DescribeCertificate(natsConn *nats.Conn, accountID string, body []byte) (*acm.DescribeCertificateOutput, error) {
	input := new(acm.DescribeCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).DescribeCertificate(input, accountID)
}

// ListCertificates — CertificateManager.ListCertificates
func ListCertificates(natsConn *nats.Conn, accountID string, body []byte) (*acm.ListCertificatesOutput, error) {
	input := new(acm.ListCertificatesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).ListCertificates(input, accountID)
}

// DeleteCertificate — CertificateManager.DeleteCertificate
func DeleteCertificate(natsConn *nats.Conn, accountID string, body []byte) (*acm.DeleteCertificateOutput, error) {
	input := new(acm.DeleteCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).DeleteCertificate(input, accountID)
}
