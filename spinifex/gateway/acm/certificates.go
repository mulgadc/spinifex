package gateway_acm

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	"github.com/nats-io/nats.go"
)

// ImportCertificate — CertificateManager.ImportCertificate.
func ImportCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.ImportCertificateOutput, error) {
	input := new(acm.ImportCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).ImportCertificate(ctx, input, accountID)
}

// DescribeCertificate — CertificateManager.DescribeCertificate.
func DescribeCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.DescribeCertificateOutput, error) {
	input := new(acm.DescribeCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).DescribeCertificate(ctx, input, accountID)
}

// ListCertificates — CertificateManager.ListCertificates.
func ListCertificates(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.ListCertificatesOutput, error) {
	input := new(acm.ListCertificatesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).ListCertificates(ctx, input, accountID)
}

// DeleteCertificate — CertificateManager.DeleteCertificate.
func DeleteCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.DeleteCertificateOutput, error) {
	input := new(acm.DeleteCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).DeleteCertificate(ctx, input, accountID)
}

// ListTagsForCertificate — CertificateManager.ListTagsForCertificate.
func ListTagsForCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.ListTagsForCertificateOutput, error) {
	input := new(acm.ListTagsForCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).ListTagsForCertificate(ctx, input, accountID)
}

// AddTagsToCertificate — CertificateManager.AddTagsToCertificate.
func AddTagsToCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.AddTagsToCertificateOutput, error) {
	input := new(acm.AddTagsToCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).AddTagsToCertificate(ctx, input, accountID)
}

// RemoveTagsFromCertificate — CertificateManager.RemoveTagsFromCertificate.
func RemoveTagsFromCertificate(ctx context.Context, natsConn *nats.Conn, accountID string, body []byte) (*acm.RemoveTagsFromCertificateOutput, error) {
	input := new(acm.RemoveTagsFromCertificateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	return handlers_acm.NewNATSACMService(natsConn).RemoveTagsFromCertificate(ctx, input, accountID)
}
