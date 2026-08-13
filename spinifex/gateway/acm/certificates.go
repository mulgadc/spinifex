package gateway_acm

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// natsTimeout bounds the gateway's wait for a daemon-side ACM response.
const natsTimeout = 30 * time.Second

// ImportCertificate — CertificateManager.ImportCertificate.
func ImportCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.ImportCertificateOutput, error) {
	input := new(acm.ImportCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.ImportCertificateOutput](ctx, natsConn, "acm.ImportCertificate", input, natsTimeout, accountID)
}

// DescribeCertificate — CertificateManager.DescribeCertificate.
func DescribeCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.DescribeCertificateOutput, error) {
	input := new(acm.DescribeCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.DescribeCertificateOutput](ctx, natsConn, "acm.DescribeCertificate", input, natsTimeout, accountID)
}

// GetCertificate — CertificateManager.GetCertificate.
func GetCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.GetCertificateOutput, error) {
	input := new(acm.GetCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.GetCertificateOutput](ctx, natsConn, "acm.GetCertificate", input, natsTimeout, accountID)
}

// ListCertificates — CertificateManager.ListCertificates.
func ListCertificates(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.ListCertificatesOutput, error) {
	input := new(acm.ListCertificatesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.ListCertificatesOutput](ctx, natsConn, "acm.ListCertificates", input, natsTimeout, accountID)
}

// DeleteCertificate — CertificateManager.DeleteCertificate.
func DeleteCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.DeleteCertificateOutput, error) {
	input := new(acm.DeleteCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.DeleteCertificateOutput](ctx, natsConn, "acm.DeleteCertificate", input, natsTimeout, accountID)
}

// ListTagsForCertificate — CertificateManager.ListTagsForCertificate.
func ListTagsForCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.ListTagsForCertificateOutput, error) {
	input := new(acm.ListTagsForCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.ListTagsForCertificateOutput](ctx, natsConn, "acm.ListTagsForCertificate", input, natsTimeout, accountID)
}

// AddTagsToCertificate — CertificateManager.AddTagsToCertificate.
func AddTagsToCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.AddTagsToCertificateOutput, error) {
	input := new(acm.AddTagsToCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.AddTagsToCertificateOutput](ctx, natsConn, "acm.AddTagsToCertificate", input, natsTimeout, accountID)
}

// RemoveTagsFromCertificate — CertificateManager.RemoveTagsFromCertificate.
func RemoveTagsFromCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.RemoveTagsFromCertificateOutput, error) {
	input := new(acm.RemoveTagsFromCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return utils.NATSRequest[acm.RemoveTagsFromCertificateOutput](ctx, natsConn, "acm.RemoveTagsFromCertificate", input, natsTimeout, accountID)
}
