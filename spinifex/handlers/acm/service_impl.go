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

// CertAuthority issues leaf certificates from the tenant private CA and
// authorizes domains against the CA's own x509 name constraints. Satisfied by
// *TenantCA (handlers/acm/privateca.go), which is implemented separately;
// defined here as a narrow interface so this package builds and is testable
// with a fake independently of that file.
type CertAuthority interface {
	// IssueLeaf signs a new leaf certificate for domain (with sans as
	// additional SANs) from the tenant CA, returning the leaf, chain and a
	// freshly generated private key as PEM.
	IssueLeaf(domain string, sans []string) (certPEM, chainPEM, keyPEM string, err error)
	// Authorized reports whether domain is within the CA's own
	// permittedDNSDomains name constraints — the sole authorization source for
	// a PRIVATE_CA request, so config and the CA can never drift apart.
	Authorized(domain string) bool
}

// ACMServiceImpl is the local, KV-backed implementation of ACMService.
type ACMServiceImpl struct {
	store  *Store
	region string

	// dnsProviderConfigured drives deriveValidationMode. Wired from
	// config.Config at construction (cfg.ACM.DNSProvider != ""); tests in this
	// package may set it directly to exercise PROVIDER_API without a full
	// config.
	dnsProviderConfigured bool

	// TenantCA issues PRIVATE_CA leaves and authorizes domains against the
	// tenant CA's name constraints. Nil until wired by the daemon
	// post-construction (mirroring CertMaterialUpdated below); RequestCertificate
	// fails loudly rather than silently skipping issuance when PRIVATE_CA is
	// derived and no CA is wired.
	TenantCA CertAuthority

	// NorthstarHostsZone, when set, reports whether northstar hosts a zone
	// covering domain — consulted once per requested domain (primary + every
	// SAN) by deriveValidationMode. Wired by the daemon post-construction
	// (mirroring CertMaterialUpdated below) to handlers/dns.HostsZone. A func
	// field stands in for a direct import of handlers/dns, the same pattern
	// CertMaterialUpdated uses for handlers/elbv2, so this package and its
	// tests stay decoupled from handlers/dns's S3/northstar-config
	// dependencies. Nil means "northstar hosts nothing" — the safe default for
	// tests and any deployment that never wires it, so an unwired service can
	// never accidentally select MANUAL_TXT for a zone it cannot actually see.
	NorthstarHostsZone func(domain string) bool

	// CertMaterialUpdated, when set, is invoked after new certificate material
	// is written under an existing ARN so the caller can re-render every load
	// balancer that references it (see handlers/elbv2.UpdateStoredConfigForCert).
	// Wired by the daemon post-construction to avoid an elbv2 -> acm import
	// cycle; nil-safe — skipped in tests and whenever ELBv2 isn't wired up.
	// Fan-out errors are logged, never propagated: a rendering problem on one
	// load balancer must not fail the certificate write that already succeeded.
	CertMaterialUpdated func(ctx context.Context, certArn string) error
}

var _ ACMService = (*ACMServiceImpl)(nil)

// NewACMServiceImplWithNATS builds an ACM service backed by a JetStream KV
// store. cfg may be nil (tests); region then falls back to the default.
// masterKey encrypts certificate private keys at rest and is required: unlike
// EKS/ECS's IAM dependency (which degrades to "feature disabled" when the key
// is missing), ACM has no safe degraded mode for a value this sensitive, so a
// missing or malformed key fails construction outright rather than starting
// the service without at-rest protection.
func NewACMServiceImplWithNATS(ctx context.Context, cfg *config.Config, nc *nats.Conn, masterKey []byte) (*ACMServiceImpl, error) {
	if len(masterKey) == 0 {
		return nil, fmt.Errorf("ACM service requires a master key to encrypt certificate private keys at rest; none provided")
	}
	store, err := NewStore(ctx, nc, masterKey)
	if err != nil {
		return nil, err
	}
	svc := &ACMServiceImpl{store: store, region: defaultRegion}
	if cfg != nil {
		if cfg.Region != "" {
			svc.region = cfg.Region
		}
		svc.dnsProviderConfigured = cfg.ACM.DNSProvider != ""
	}
	return svc, nil
}

// deriveValidationMode selects a validation mode from deployment state and the
// full set of requested domains (primary + every SAN), never from
// configuration alone or from the primary domain in isolation.
//
// Order, and why:
//  1. PRIVATE_CA, when a tenant CA is wired and its name constraints authorize
//     every requested domain. A name-constrained tenant CA is a definite,
//     operator-declared local capability, so it is preferred even over
//     northstar hosting the zone: northstar hosting a zone is evidence the
//     zone is internal, and an internal zone can never complete MANUAL_TXT (a
//     public CA cannot resolve it there) — PRIVATE_CA is the only mode that
//     can actually issue for it.
//  2. PROVIDER_API, when a DNS provider credential is configured. An operator
//     who has wired provider credentials wants public trust driven
//     automatically; that must not be downgraded just because northstar also
//     happens to host the zone.
//  3. MANUAL_TXT, when northstar hosts a zone covering every requested domain
//     and neither of the above applied. This is last, not first: MANUAL_TXT
//     means "the operator hand-publishes an ACME TXT record for a public CA to
//     resolve", which can never complete for an internal zone — it is only
//     offered once PRIVATE_CA, the mode that actually works there, has been
//     ruled out.
//  4. PRIVATE_CA otherwise — the same terminal fallback as before, which then
//     fails the authorization check in RequestCertificate loudly when no
//     tenant CA is wired or it does not cover the domain.
//
// Coverage in (1) and (3) is all-or-nothing across every domain: a request
// with mixed coverage (some domains covered, some not) must not select a mode
// it cannot actually serve for the domains it misses.
// CNAME_DELEGATION is never selected here — it is deferred until northstar can
// serve public authoritative queries, so northstar hosting the zone takes the
// manual-TXT branch of that family for now.
func (s *ACMServiceImpl) deriveValidationMode(domains []string) string {
	switch {
	case s.TenantCA != nil && allAuthorized(s.TenantCA, domains):
		return ValidationModePrivateCA
	case s.dnsProviderConfigured:
		return ValidationModeProviderAPI
	case s.northstarHostsAll(domains):
		return ValidationModeManualTXT
	default:
		return ValidationModePrivateCA
	}
}

// allAuthorized reports whether ca authorizes every domain in domains. ca must
// be non-nil; domains must be non-empty (an empty set authorizes nothing).
func allAuthorized(ca CertAuthority, domains []string) bool {
	if len(domains) == 0 {
		return false
	}
	for _, d := range domains {
		if !ca.Authorized(d) {
			return false
		}
	}
	return true
}

// northstarHostsAll reports whether NorthstarHostsZone covers every domain in
// domains. A nil NorthstarHostsZone (unwired service, or a test that never set
// it) means northstar hosts nothing.
func (s *ACMServiceImpl) northstarHostsAll(domains []string) bool {
	if s.NorthstarHostsZone == nil || len(domains) == 0 {
		return false
	}
	for _, d := range domains {
		if !s.NorthstarHostsZone(d) {
			return false
		}
	}
	return true
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
	var inUseBy []string
	reimport := certArn != ""
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
		// Carry the InUseBy index forward — new material under the same ARN
		// must not silently drop the load balancers that reference it.
		inUseBy = existing.InUseBy
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
		InUseBy:          inUseBy,
		Type:             certTypeImported,
		Status:           certStatusIssued,
		// Imported material has no validation and is never auto-renewed —
		// re-importing (and thus re-proving control) is the operator's job.
		RenewalEligibility: acm.RenewalEligibilityIneligible,
	}
	if err := s.store.PutCert(ctx, rec); err != nil {
		slog.ErrorContext(ctx, "ImportCertificate: store failed", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// Fan the new material out to every load balancer already referencing this
	// ARN so HAProxy picks it up on its next poll instead of silently serving
	// the old leaf until it expires. Zero load balancers in use is a no-op.
	if reimport && len(inUseBy) > 0 && s.CertMaterialUpdated != nil {
		if fanErr := s.CertMaterialUpdated(ctx, certArn); fanErr != nil {
			slog.ErrorContext(ctx, "ImportCertificate: fan-out to load balancers failed", "arn", certArn, "err", fanErr)
		}
	}

	slog.InfoContext(ctx, "ImportCertificate: stored", "arn", certArn, "domain", rec.DomainName, "account", accountID)
	return &acm.ImportCertificateOutput{CertificateArn: aws.String(certArn)}, nil
}

// RequestCertificate mints a CertificateArn and returns immediately with the
// certificate in PENDING_VALIDATION — it never performs issuance inline,
// matching AWS. The validation mode is derived (deriveValidationMode), not
// read from the request. Only PRIVATE_CA is driven to completion in this
// slice: it has no domain validation, so it issues synchronously against
// TenantCA and reaches ISSUED before this call returns. PROVIDER_API and
// MANUAL_TXT requests are stored correctly shaped in PENDING_VALIDATION and
// left for a later issuance worker to drive.
func (s *ACMServiceImpl) RequestCertificate(ctx context.Context, input *acm.RequestCertificateInput, accountID string) (*acm.RequestCertificateOutput, error) {
	if input == nil || aws.StringValue(input.DomainName) == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameter)
	}
	domain := aws.StringValue(input.DomainName)
	sans := aws.StringValueSlice(input.SubjectAlternativeNames)
	allDomains := uniqueDomains(domain, sans)

	mode := s.deriveValidationMode(allDomains)

	// PRIVATE_CA has no ACME CA to prove domain control, so request-time
	// authorization is read from the tenant CA's own name constraints — the
	// sole source, so it and this check can never drift apart. Public modes
	// need no equivalent check: the ACME CA's own validation is a stronger
	// proof of control than anything Spinifex could assert itself.
	if mode == ValidationModePrivateCA {
		if s.TenantCA == nil {
			slog.ErrorContext(ctx, "RequestCertificate: PRIVATE_CA mode derived but no tenant CA is wired", "domain", domain)
			return nil, errors.New(awserrors.ErrorInternalError)
		}
		for _, d := range allDomains {
			if !s.TenantCA.Authorized(d) {
				slog.InfoContext(ctx, "RequestCertificate: domain outside tenant CA permitted domains", "domain", d)
				return nil, errors.New(awserrors.ErrorInvalidParameter)
			}
		}
	}

	certArn := s.mintCertificateArn(accountID)
	rec := &CertRecord{
		CertificateArn:          certArn,
		AccountID:               accountID,
		DomainName:              domain,
		SubjectAltNames:         sans,
		Status:                  acm.CertificateStatusPendingValidation,
		ValidationMethod:        mode,
		DomainValidationOptions: buildDomainValidationOptions(mode, allDomains),
		RenewalEligibility:      renewalEligibilityForMode(mode),
		// Minted now regardless of mode so CNAME_DELEGATION, when it lands, has
		// a stable target without changing an existing certificate's validation.
		DelegationToken: uuid.NewString(),
		Tags:            tagsToMap(input.Tags),
	}

	if mode == ValidationModePrivateCA {
		rec.Type = acm.CertificateTypePrivate
		if err := s.issuePrivateCALeaf(ctx, rec, domain, sans); err != nil {
			return nil, err
		}
	} else {
		rec.Type = acm.CertificateTypeAmazonIssued
	}

	if err := s.store.PutCert(ctx, rec); err != nil {
		slog.ErrorContext(ctx, "RequestCertificate: store failed", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	slog.InfoContext(ctx, "RequestCertificate: stored", "arn", certArn, "domain", domain, "mode", mode, "status", rec.Status)
	return &acm.RequestCertificateOutput{CertificateArn: aws.String(certArn)}, nil
}

// issuePrivateCALeaf signs rec's leaf from TenantCA and advances rec straight
// to ISSUED — PRIVATE_CA has no validation step to wait on. Caller has already
// checked TenantCA is non-nil and every domain is authorized.
func (s *ACMServiceImpl) issuePrivateCALeaf(ctx context.Context, rec *CertRecord, domain string, sans []string) error {
	certPEM, chainPEM, keyPEM, err := s.TenantCA.IssueLeaf(domain, sans)
	if err != nil {
		slog.ErrorContext(ctx, "RequestCertificate: private CA issuance failed", "domain", domain, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}
	leaf, err := parseLeaf([]byte(certPEM))
	if err != nil {
		slog.ErrorContext(ctx, "RequestCertificate: private CA returned an unparseable leaf", "domain", domain, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	rec.Certificate = certPEM
	rec.CertificateChain = chainPEM
	rec.PrivateKey = keyPEM
	rec.Serial = leaf.SerialNumber.Text(16)
	rec.Subject = leaf.Subject.String()
	rec.Issuer = leaf.Issuer.String()
	rec.KeyAlgorithm = keyAlgorithm(leaf)
	rec.NotBefore = leaf.NotBefore
	rec.NotAfter = leaf.NotAfter
	rec.Status = acm.CertificateStatusIssued
	for i := range rec.DomainValidationOptions {
		rec.DomainValidationOptions[i].ValidationStatus = acm.DomainStatusSuccess
	}
	return nil
}

// uniqueDomains returns domain plus sans, deduplicated and order-preserved —
// the full set of names a certificate's per-domain validation state and
// authorization checks must cover.
func uniqueDomains(domain string, sans []string) []string {
	out := make([]string, 0, 1+len(sans))
	seen := make(map[string]bool, 1+len(sans))
	for _, d := range append([]string{domain}, sans...) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// buildDomainValidationOptions shapes one DomainValidationEntry per domain for
// the derived mode. Only MANUAL_TXT gets a ResourceRecord: PROVIDER_API and
// PRIVATE_CA both leave it empty because Spinifex (or, for PRIVATE_CA, nobody)
// owns the record write, and the caller has nothing to publish. The TXT
// record's Name is deterministic ("_acme-challenge.<domain>."), but its Value
// is the per-order ACME challenge token, which does not exist until a later
// slice's worker places the order — so it stays empty here.
func buildDomainValidationOptions(mode string, domains []string) []DomainValidationEntry {
	entries := make([]DomainValidationEntry, 0, len(domains))
	for _, d := range domains {
		e := DomainValidationEntry{DomainName: d, ValidationStatus: acm.DomainStatusPendingValidation}
		// PRIVATE_CA validates nothing, so it carries no method — matching real
		// ACM on a private certificate, where the AWS provider reads the method
		// back as NONE whatever the request asked for.
		if mode != ValidationModePrivateCA {
			e.ValidationMethod = acm.ValidationMethodDns
		}
		if mode == ValidationModeManualTXT {
			e.RecordType = "TXT"
			e.RecordName = "_acme-challenge." + d + "."
		}
		entries = append(entries, e)
	}
	return entries
}

// renewalEligibilityForMode reports whether the future renewal worker may
// reissue a managed certificate under its existing ARN. MANUAL_TXT is
// INELIGIBLE because its challenge token rotates per order, making unattended
// renewal impossible; marking it now keeps that worker from thrashing against
// a certificate it can never renew.
func renewalEligibilityForMode(mode string) string {
	if mode == ValidationModeManualTXT {
		return acm.RenewalEligibilityIneligible
	}
	return acm.RenewalEligibilityEligible
}

// domainValidationOptionsToAWS converts stored DomainValidationEntry records
// to the AWS-shaped []*acm.DomainValidation. An entry with no RecordType
// carries DomainName, ValidationMethod and ValidationStatus but no
// ResourceRecord — real ACM omits the field the same way until it has
// something to report, so this is a legitimate AWS-shaped state rather than a
// partial one.
func domainValidationOptionsToAWS(entries []DomainValidationEntry) []*acm.DomainValidation {
	if len(entries) == 0 {
		return nil
	}
	out := make([]*acm.DomainValidation, 0, len(entries))
	for _, e := range entries {
		dv := &acm.DomainValidation{
			DomainName:       aws.String(e.DomainName),
			ValidationStatus: aws.String(e.ValidationStatus),
		}
		if e.ValidationMethod != "" {
			dv.ValidationMethod = aws.String(e.ValidationMethod)
		}
		if e.RecordType != "" {
			dv.ResourceRecord = &acm.ResourceRecord{
				Name:  aws.String(e.RecordName),
				Type:  aws.String(e.RecordType),
				Value: aws.String(e.RecordValue),
			}
		}
		out = append(out, dv)
	}
	return out
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

// GetCertificate returns the PEM-encoded certificate body and chain for an
// owned ARN. This is the only ACM read that hands back the certificate itself
// rather than metadata about it — it is what a client pinning the issuing chain
// fetches, and what Terraform reads to feed a certificate into another
// resource. It returns no key material: ACM discloses none through any API.
func (s *ACMServiceImpl) GetCertificate(ctx context.Context, input *acm.GetCertificateInput, accountID string) (*acm.GetCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	// A requested certificate has a record before it has a body. Returning an
	// empty string here would read as a successfully fetched empty certificate,
	// so a not-yet-issued cert is a distinct retryable error, as in AWS.
	if rec.Certificate == "" {
		return nil, errors.New(awserrors.ErrorACMRequestInProgress)
	}
	out := &acm.GetCertificateOutput{Certificate: aws.String(rec.Certificate)}
	// Left absent rather than empty for a self-signed leaf with no chain.
	if rec.CertificateChain != "" {
		out.CertificateChain = aws.String(rec.CertificateChain)
	}
	return out, nil
}

// ListCertificates returns summaries for every cert owned by accountID.
// Summaries carry no key material, so this takes the metadata-only accessor.
func (s *ACMServiceImpl) ListCertificates(ctx context.Context, input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error) {
	recs, err := s.store.ListCertMetadata(ctx, accountID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	out := &acm.ListCertificatesOutput{}
	for _, rec := range recs {
		out.CertificateSummaryList = append(out.CertificateSummaryList, &acm.CertificateSummary{
			CertificateArn: aws.String(rec.CertificateArn),
			DomainName:     aws.String(rec.DomainName),
			Status:         aws.String(certStatusOrDefault(rec)),
			Type:           aws.String(certTypeOrDefault(rec)),
			KeyAlgorithm:   aws.String(rec.KeyAlgorithm),
			NotBefore:      timePtr(rec.NotBefore),
			NotAfter:       timePtr(rec.NotAfter),
			ImportedAt:     timePtr(rec.ImportedAt),
			InUse:          aws.Bool(len(rec.InUseBy) > 0),
		})
	}
	return out, nil
}

// DeleteCertificate removes an owned cert; unknown ARN → ResourceNotFound.
// Matches AWS: a certificate still referenced by a load balancer listener
// (InUseBy non-empty) is refused with ResourceInUseException, no force flag.
// Without this, a destroy on a spine workspace could remove a shared
// certificate out from under every HTTPS listener still using it.
func (s *ACMServiceImpl) DeleteCertificate(ctx context.Context, input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error) {
	if input == nil || aws.StringValue(input.CertificateArn) == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := s.lookupOwned(ctx, aws.StringValue(input.CertificateArn), accountID)
	if err != nil {
		return nil, err
	}
	if len(rec.InUseBy) > 0 {
		slog.InfoContext(ctx, "DeleteCertificate: refused, certificate in use", "arn", rec.CertificateArn, "in_use_by", rec.InUseBy)
		return nil, errors.New(awserrors.ErrorACMResourceInUse)
	}
	if _, err := s.store.DeleteCert(ctx, aws.StringValue(input.CertificateArn)); err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return &acm.DeleteCertificateOutput{}, nil
}

// lookupOwned fetches a cert by ARN, returning ResourceNotFound when absent or
// owned by a different account (no cross-account disclosure).
//
// Takes the metadata read: no caller of this helper reads PrivateKey, so an
// ownership check must not decrypt one. A caller that does need the key
// fetches it explicitly via store.GetCert once ownership is established.
func (s *ACMServiceImpl) lookupOwned(ctx context.Context, certArn, accountID string) (*CertRecord, error) {
	rec, err := s.store.GetCertMetadata(ctx, certArn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if rec == nil || rec.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorResourceNotFound)
	}
	return rec, nil
}

// recordToDetail is the only surface on which Terraform, the CLI and
// spinifex-ui learn why a certificate is stuck, so Type/Status/
// DomainValidationOptions/FailureReason/RenewalEligibility/RenewalSummary are
// functional requirements, not cosmetics. It never sets a PrivateKey field —
// CertificateDetail has none; ACM never returns key material from Describe.
func recordToDetail(rec *CertRecord) *acm.CertificateDetail {
	detail := &acm.CertificateDetail{
		CertificateArn:     aws.String(rec.CertificateArn),
		DomainName:         aws.String(rec.DomainName),
		Serial:             aws.String(rec.Serial),
		Subject:            aws.String(rec.Subject),
		Issuer:             aws.String(rec.Issuer),
		KeyAlgorithm:       aws.String(rec.KeyAlgorithm),
		Status:             aws.String(certStatusOrDefault(rec)),
		Type:               aws.String(certTypeOrDefault(rec)),
		NotBefore:          timePtr(rec.NotBefore),
		NotAfter:           timePtr(rec.NotAfter),
		ImportedAt:         timePtr(rec.ImportedAt),
		InUseBy:            aws.StringSlice(rec.InUseBy),
		RenewalEligibility: aws.String(renewalEligibilityOrDefault(rec)),
	}
	if len(rec.SubjectAltNames) > 0 {
		detail.SubjectAlternativeNames = aws.StringSlice(rec.SubjectAltNames)
	}
	if len(rec.DomainValidationOptions) > 0 {
		detail.DomainValidationOptions = domainValidationOptionsToAWS(rec.DomainValidationOptions)
	}
	if rec.FailureReason != "" {
		detail.FailureReason = aws.String(rec.FailureReason)
	}
	if rec.RenewalSummary != nil {
		summary := &acm.RenewalSummary{
			DomainValidationOptions: domainValidationOptionsToAWS(rec.DomainValidationOptions),
			RenewalStatus:           aws.String(rec.RenewalSummary.RenewalStatus),
			UpdatedAt:               timePtr(rec.RenewalSummary.UpdatedAt),
		}
		if rec.RenewalSummary.RenewalStatusReason != "" {
			summary.RenewalStatusReason = aws.String(rec.RenewalSummary.RenewalStatusReason)
		}
		detail.RenewalSummary = summary
	}
	return detail
}

// certStatusOrDefault and certTypeOrDefault fall back to the pre-managed-
// issuance defaults (ISSUED/IMPORTED) for records written before Type/Status
// existed on CertRecord — every certificate stored before this change was an
// immediately-issued import.
func certStatusOrDefault(rec *CertRecord) string {
	if rec.Status != "" {
		return rec.Status
	}
	return certStatusIssued
}

func certTypeOrDefault(rec *CertRecord) string {
	if rec.Type != "" {
		return rec.Type
	}
	return certTypeImported
}

// renewalEligibilityOrDefault falls back to INELIGIBLE: unset covers both
// legacy imported records (predating this field) and freshly imported ones,
// and imported certificates are never auto-renewed either way.
func renewalEligibilityOrDefault(rec *CertRecord) string {
	if rec.RenewalEligibility != "" {
		return rec.RenewalEligibility
	}
	return acm.RenewalEligibilityIneligible
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
