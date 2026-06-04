package handlers_acm

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/nats-io/nats.go"
)

const (
	defaultRegion = "ap-southeast-2"

	certStatusIssued = "ISSUED"
	certTypeImported = "IMPORTED"
)

// ACMServiceImpl is the local, KV-backed implementation of ACMService.
type ACMServiceImpl struct {
	store  *Store
	region string
}

var _ ACMService = (*ACMServiceImpl)(nil)

// NewACMServiceImplWithNATS builds an ACM service backed by a JetStream KV
// store. cfg may be nil (tests); region then falls back to the default.
func NewACMServiceImplWithNATS(cfg *config.Config, nc *nats.Conn) (*ACMServiceImpl, error) {
	store, err := NewStore(nc)
	if err != nil {
		return nil, err
	}
	region := defaultRegion
	if cfg != nil && cfg.Region != "" {
		region = cfg.Region
	}
	return &ACMServiceImpl{store: store, region: region}, nil
}

// mintCertificateArn generates an ACM-style certificate ARN for accountID.
func (s *ACMServiceImpl) mintCertificateArn(accountID string) string {
	return fmt.Sprintf("arn:aws:acm:%s:%s:certificate/%s", s.region, accountID, uuid.NewString())
}

// ImportCertificate validates the PEM material, parses the leaf for metadata,
// and stores it under a new (or, on re-import, the supplied) ACM ARN.
func (s *ACMServiceImpl) ImportCertificate(input *acm.ImportCertificateInput, accountID string) (*acm.ImportCertificateOutput, error) {
	if input == nil || len(input.Certificate) == 0 || len(input.PrivateKey) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}

	// The leaf cert and private key must form a valid keypair.
	if _, err := tls.X509KeyPair(input.Certificate, input.PrivateKey); err != nil {
		slog.Debug("ImportCertificate: keypair validation failed", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}

	leaf, err := parseLeaf(input.Certificate)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}

	certArn := aws.StringValue(input.CertificateArn)
	if certArn == "" {
		certArn = s.mintCertificateArn(accountID)
	} else {
		// Re-import: the ARN must already exist and belong to the caller.
		existing, gErr := s.store.GetCert(certArn)
		if gErr != nil {
			return nil, errors.New(awserrors.ErrorInternalError)
		}
		if existing == nil || existing.AccountID != accountID {
			return nil, errors.New(awserrors.ErrorResourceNotFound)
		}
	}

	rec := &CertRecord{
		CertificateArn:   certArn,
		AccountID:        accountID,
		Certificate:      string(input.Certificate),
		CertificateChain: string(input.CertificateChain),
		PrivateKey:       string(input.PrivateKey),
		DomainName:       leafDomain(leaf),
		SubjectAltNames:  leaf.DNSNames,
		Serial:           leaf.SerialNumber.Text(16),
		Subject:          leaf.Subject.String(),
		Issuer:           leaf.Issuer.String(),
		KeyAlgorithm:     keyAlgorithm(leaf),
		NotBefore:        leaf.NotBefore,
		NotAfter:         leaf.NotAfter,
		ImportedAt:       time.Now().UTC(),
	}
	if err := s.store.PutCert(rec); err != nil {
		slog.Error("ImportCertificate: store failed", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	slog.Info("ImportCertificate: stored", "arn", certArn, "domain", rec.DomainName, "account", accountID)
	return &acm.ImportCertificateOutput{CertificateArn: aws.String(certArn)}, nil
}

// DescribeCertificate returns the CertificateDetail for an owned ARN.
func (s *ACMServiceImpl) DescribeCertificate(input *acm.DescribeCertificateInput, accountID string) (*acm.DescribeCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	return &acm.DescribeCertificateOutput{Certificate: recordToDetail(rec)}, nil
}

// ListCertificates returns summaries for every cert owned by accountID.
func (s *ACMServiceImpl) ListCertificates(input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error) {
	recs, err := s.store.ListCerts(accountID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	out := &acm.ListCertificatesOutput{}
	for _, rec := range recs {
		out.CertificateSummaryList = append(out.CertificateSummaryList, &acm.CertificateSummary{
			CertificateArn: aws.String(rec.CertificateArn),
			DomainName:     aws.String(rec.DomainName),
			Status:         aws.String(certStatusIssued),
			Type:           aws.String(certTypeImported),
			KeyAlgorithm:   aws.String(rec.KeyAlgorithm),
			NotBefore:      timePtr(rec.NotBefore),
			NotAfter:       timePtr(rec.NotAfter),
			ImportedAt:     timePtr(rec.ImportedAt),
			InUse:          aws.Bool(false),
		})
	}
	return out, nil
}

// DeleteCertificate removes an owned cert; unknown ARN → ResourceNotFound.
func (s *ACMServiceImpl) DeleteCertificate(input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	if _, err := s.lookupOwned(aws.StringValue(input.CertificateArn), accountID); err != nil {
		return nil, err
	}
	if _, err := s.store.DeleteCert(aws.StringValue(input.CertificateArn)); err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return &acm.DeleteCertificateOutput{}, nil
}

// lookupOwned fetches a cert by ARN, returning ResourceNotFound when absent or
// owned by a different account (no cross-account disclosure).
func (s *ACMServiceImpl) lookupOwned(certArn, accountID string) (*CertRecord, error) {
	rec, err := s.store.GetCert(certArn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if rec == nil || rec.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorResourceNotFound)
	}
	return rec, nil
}

func recordToDetail(rec *CertRecord) *acm.CertificateDetail {
	detail := &acm.CertificateDetail{
		CertificateArn: aws.String(rec.CertificateArn),
		DomainName:     aws.String(rec.DomainName),
		Serial:         aws.String(rec.Serial),
		Subject:        aws.String(rec.Subject),
		Issuer:         aws.String(rec.Issuer),
		KeyAlgorithm:   aws.String(rec.KeyAlgorithm),
		Status:         aws.String(certStatusIssued),
		Type:           aws.String(certTypeImported),
		NotBefore:      timePtr(rec.NotBefore),
		NotAfter:       timePtr(rec.NotAfter),
		ImportedAt:     timePtr(rec.ImportedAt),
		InUseBy:        []*string{},
	}
	if len(rec.SubjectAltNames) > 0 {
		detail.SubjectAlternativeNames = aws.StringSlice(rec.SubjectAltNames)
	}
	return detail
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// parseLeaf decodes the first CERTIFICATE block from certPEM and parses it.
func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no PEM CERTIFICATE block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// leafDomain returns the certificate's primary domain: CommonName, else the
// first SAN, else empty.
func leafDomain(leaf *x509.Certificate) string {
	if cn := strings.TrimSpace(leaf.Subject.CommonName); cn != "" {
		return cn
	}
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0]
	}
	return ""
}

// keyAlgorithm maps the leaf public key to an ACM-style algorithm string
// (RSA_2048, EC_prime256v1, ...).
func keyAlgorithm(leaf *x509.Certificate) string {
	switch pub := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA_%d", pub.N.BitLen())
	case *ecdsa.PublicKey:
		return "EC_" + pub.Curve.Params().Name
	default:
		return "UNKNOWN"
	}
}
