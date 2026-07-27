package handlers_acm

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
func NewACMServiceImplWithNATS(ctx context.Context, cfg *config.Config, nc *nats.Conn) (*ACMServiceImpl, error) {
	store, err := NewStore(ctx, nc)
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
func (s *ACMServiceImpl) ImportCertificate(ctx context.Context, input *acm.ImportCertificateInput, accountID string) (*acm.ImportCertificateOutput, error) {
	if input == nil || len(input.Certificate) == 0 || len(input.PrivateKey) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}

	// The leaf cert and private key must form a valid keypair.
	if _, err := tls.X509KeyPair(input.Certificate, input.PrivateKey); err != nil {
		slog.DebugContext(ctx, "ImportCertificate: keypair validation failed", "err", err)
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
		existing, gErr := s.store.GetCert(ctx, certArn)
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
		Tags:             tagsToMap(input.Tags),
	}
	if err := s.store.PutCert(ctx, rec); err != nil {
		slog.ErrorContext(ctx, "ImportCertificate: store failed", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	slog.InfoContext(ctx, "ImportCertificate: stored", "arn", certArn, "domain", rec.DomainName, "account", accountID)
	return &acm.ImportCertificateOutput{CertificateArn: aws.String(certArn)}, nil
}

// DescribeCertificate returns the CertificateDetail for an owned ARN.
func (s *ACMServiceImpl) DescribeCertificate(ctx context.Context, input *acm.DescribeCertificateInput, accountID string) (*acm.DescribeCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	return &acm.DescribeCertificateOutput{Certificate: recordToDetail(rec)}, nil
}

// ListCertificates returns summaries for every cert owned by accountID.
func (s *ACMServiceImpl) ListCertificates(ctx context.Context, input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error) {
	recs, err := s.store.ListCerts(ctx, accountID)
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
func (s *ACMServiceImpl) DeleteCertificate(ctx context.Context, input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	if _, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID); err != nil {
		return nil, err
	}
	if _, err := s.store.DeleteCert(ctx, aws.StringValue(input.CertificateArn)); err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return &acm.DeleteCertificateOutput{}, nil
}

// lookupOwned fetches a cert by ARN, returning ResourceNotFound when absent or
// owned by a different account (no cross-account disclosure).
func (s *ACMServiceImpl) lookupOwned(ctx context.Context, certArn, accountID string) (*CertRecord, error) {
	rec, err := s.store.GetCert(ctx, certArn)
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

// ListTagsForCertificate returns the tags stored on an owned certificate.
func (s *ACMServiceImpl) ListTagsForCertificate(ctx context.Context, input *acm.ListTagsForCertificateInput, accountID string) (*acm.ListTagsForCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	return &acm.ListTagsForCertificateOutput{Tags: mapToTags(rec.Tags)}, nil
}

// AddTagsToCertificate merges the supplied tags onto an owned certificate.
func (s *ACMServiceImpl) AddTagsToCertificate(ctx context.Context, input *acm.AddTagsToCertificateInput, accountID string) (*acm.AddTagsToCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	if rec.Tags == nil {
		rec.Tags = map[string]string{}
	}
	maps.Copy(rec.Tags, tagsToMap(input.Tags))
	if err := s.store.PutCert(ctx, rec); err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return &acm.AddTagsToCertificateOutput{}, nil
}

// RemoveTagsFromCertificate deletes the named tags from an owned certificate. A
// tag with a nil value matches by key; a non-nil value removes only on an exact
// value match, mirroring ACM semantics.
func (s *ACMServiceImpl) RemoveTagsFromCertificate(ctx context.Context, input *acm.RemoveTagsFromCertificateInput, accountID string) (*acm.RemoveTagsFromCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	for _, tag := range input.Tags {
		if tag == nil {
			continue
		}
		key := aws.StringValue(tag.Key)
		if tag.Value != nil && rec.Tags[key] != aws.StringValue(tag.Value) {
			continue
		}
		delete(rec.Tags, key)
	}
	if err := s.store.PutCert(ctx, rec); err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return &acm.RemoveTagsFromCertificateOutput{}, nil
}

// tagsToMap converts ACM SDK tags to a key/value map, dropping nil entries.
func tagsToMap(tags []*acm.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t == nil || t.Key == nil {
			continue
		}
		out[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapToTags converts a key/value map back to ACM SDK tags.
func mapToTags(m map[string]string) []*acm.Tag {
	if len(m) == 0 {
		return []*acm.Tag{}
	}
	out := make([]*acm.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, &acm.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
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
