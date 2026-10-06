package handlers_acm

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
)

// maxSANSummaries is how many names AWS lists in a ListCertificates summary
// before setting HasAdditionalSubjectAlternativeNames.
const maxSANSummaries = 100

var (
	oidExtensionKeyUsage         = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidExtensionExtendedKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 37}
)

// formatSerial renders a serial number as ACM does: two lowercase hex digits
// per byte, joined by colons (66:dd:b7:...).
func formatSerial(serial *big.Int) string {
	b := serial.Bytes()
	parts := make([]string, len(b))
	for i := range b {
		parts[i] = hex.EncodeToString(b[i : i+1])
	}
	return strings.Join(parts, ":")
}

// issuerName returns the issuer's CN when the issuer DN holds nothing else,
// which is how ACM shows a self-signed import. Any richer DN keeps its full
// string form, since what ACM shows for one has not been observed.
func issuerName(leaf *x509.Certificate) string {
	if leaf.Issuer.CommonName != "" && len(leaf.Issuer.Names) == 1 {
		return leaf.Issuer.CommonName
	}
	return leaf.Issuer.String()
}

// importedSANs is the SubjectAlternativeNames ACM reports for an imported
// certificate: its DNS SANs, or its CN when it carries no SAN extension.
func importedSANs(leaf *x509.Certificate) []string {
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames
	}
	if d := leafDomain(leaf); d != "" {
		return []string{d}
	}
	return nil
}

// signatureAlgorithmName maps the leaf's signature algorithm to ACM's
// spelling, or "" for an algorithm whose ACM spelling has not been observed.
func signatureAlgorithmName(leaf *x509.Certificate) string {
	if leaf.SignatureAlgorithm == x509.SHA256WithRSA {
		return "SHA256WITHRSA"
	}
	return ""
}

func hasExtension(leaf *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	for _, ext := range leaf.Extensions {
		if ext.Id.Equal(oid) {
			return true
		}
	}
	return false
}

// parseStoredLeaf parses the stored certificate body, or returns nil for a
// record that has none yet (a requested certificate awaiting issuance).
func parseStoredLeaf(rec *CertRecord) *x509.Certificate {
	if rec.Certificate == "" {
		return nil
	}
	leaf, err := parseLeaf([]byte(rec.Certificate))
	if err != nil {
		slog.Warn("acm: stored certificate body does not parse; describing it from stored fields only", "arn", rec.CertificateArn, "err", err)
		return nil
	}
	return leaf
}

// importedCreatedAt falls back to ImportedAt for a record stored before
// CreatedAt was kept; for those, the first import time is no longer known.
func importedCreatedAt(rec *CertRecord) time.Time {
	if !rec.CreatedAt.IsZero() {
		return rec.CreatedAt
	}
	return rec.ImportedAt
}

// applyLeafDetail fills the CertificateDetail fields that come from the
// certificate body. They are derived on read, so records stored before these
// fields existed describe the same as new ones.
func applyLeafDetail(detail *acm.CertificateDetail, rec *CertRecord, leaf *x509.Certificate) {
	if leaf != nil {
		detail.Serial = aws.String(formatSerial(leaf.SerialNumber))
		if alg := signatureAlgorithmName(leaf); alg != "" {
			detail.SignatureAlgorithm = aws.String(alg)
		}
	}
	if certTypeOrDefault(rec) != certTypeImported {
		return
	}
	detail.CreatedAt = timePtr(importedCreatedAt(rec))
	detail.Options = &acm.CertificateOptions{
		CertificateTransparencyLoggingPreference: aws.String(acm.CertificateTransparencyLoggingPreferenceDisabled),
	}
	if leaf == nil {
		return
	}
	detail.Issuer = aws.String(issuerName(leaf))
	if sans := importedSANs(leaf); len(sans) > 0 {
		detail.SubjectAlternativeNames = aws.StringSlice(sans)
		dvos := make([]*acm.DomainValidation, 0, len(sans))
		for _, d := range sans {
			dvos = append(dvos, &acm.DomainValidation{DomainName: aws.String(d)})
		}
		detail.DomainValidationOptions = dvos
	}
	if !hasExtension(leaf, oidExtensionKeyUsage) {
		detail.KeyUsages = []*acm.KeyUsage{{Name: aws.String(acm.KeyUsageNameAny)}}
	}
	if !hasExtension(leaf, oidExtensionExtendedKeyUsage) {
		detail.ExtendedKeyUsages = []*acm.ExtendedKeyUsage{{Name: aws.String(acm.ExtendedKeyUsageNameNone), OID: aws.String("")}}
	}
}

// applyLeafSummary fills the ListCertificates summary fields ACM reports for
// an imported certificate, derived on read as in applyLeafDetail.
func applyLeafSummary(sum *acm.CertificateSummary, rec *CertRecord, leaf *x509.Certificate) {
	if certTypeOrDefault(rec) != certTypeImported {
		return
	}
	sum.CreatedAt = timePtr(importedCreatedAt(rec))
	if leaf == nil {
		return
	}
	if sans := importedSANs(leaf); len(sans) > 0 {
		shown := sans[:min(len(sans), maxSANSummaries)]
		sum.SubjectAlternativeNameSummaries = aws.StringSlice(shown)
		sum.HasAdditionalSubjectAlternativeNames = aws.Bool(len(sans) > maxSANSummaries)
	}
	if !hasExtension(leaf, oidExtensionKeyUsage) {
		sum.KeyUsages = aws.StringSlice([]string{acm.KeyUsageNameAny})
	}
	if !hasExtension(leaf, oidExtensionExtendedKeyUsage) {
		sum.ExtendedKeyUsages = aws.StringSlice([]string{acm.ExtendedKeyUsageNameNone})
	}
}
