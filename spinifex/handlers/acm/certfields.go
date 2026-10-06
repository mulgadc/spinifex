package handlers_acm

import (
	"crypto/x509"
	"fmt"
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

// formatSerial renders a serial number as ACM does: two lowercase hex digits
// per byte, joined by colons.
func formatSerial(serial *big.Int) string {
	b := serial.Bytes()
	if len(b) == 0 {
		b = []byte{0}
	}
	return strings.ReplaceAll(fmt.Sprintf("% x", b), " ", ":")
}

// issuerName returns the issuer's CN when the issuer DN holds nothing else,
// as ACM shows a self-signed import. A richer DN keeps its full string form,
// since what ACM shows for one has not been observed.
func issuerName(leaf *x509.Certificate) string {
	if leaf.Issuer.CommonName != "" && len(leaf.Issuer.Names) == 1 {
		return leaf.Issuer.CommonName
	}
	return leaf.Issuer.String()
}

// importedSANs is what ACM reports for an imported certificate: its DNS SANs,
// or its CN when it carries no SAN extension.
func importedSANs(leaf *x509.Certificate) []string {
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames
	}
	if d := leafDomain(leaf); d != "" {
		return []string{d}
	}
	return nil
}

// parseStoredLeaf parses the stored certificate body, or returns nil for a
// record that has none yet (a requested certificate awaiting issuance).
func parseStoredLeaf(rec *CertRecord) *x509.Certificate {
	if rec.Certificate == "" {
		return nil
	}
	leaf, err := parseLeaf([]byte(rec.Certificate))
	if err != nil {
		slog.Warn("acm: stored certificate body does not parse", "arn", rec.CertificateArn, "err", err)
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
func applyLeafDetail(detail *acm.CertificateDetail, rec *CertRecord) {
	leaf := parseStoredLeaf(rec)
	if leaf != nil {
		detail.Serial = aws.String(formatSerial(leaf.SerialNumber))
		detail.Issuer = aws.String(issuerName(leaf))
		if leaf.SignatureAlgorithm == x509.SHA256WithRSA {
			detail.SignatureAlgorithm = aws.String("SHA256WITHRSA")
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
	if sans := importedSANs(leaf); len(sans) > 0 {
		detail.SubjectAlternativeNames = aws.StringSlice(sans)
		detail.DomainValidationOptions = make([]*acm.DomainValidation, len(sans))
		for i, d := range sans {
			detail.DomainValidationOptions[i] = &acm.DomainValidation{DomainName: aws.String(d)}
		}
	}
	if leaf.KeyUsage == 0 {
		detail.KeyUsages = []*acm.KeyUsage{{Name: aws.String(acm.KeyUsageNameAny)}}
	}
	if len(leaf.ExtKeyUsage)+len(leaf.UnknownExtKeyUsage) == 0 {
		detail.ExtendedKeyUsages = []*acm.ExtendedKeyUsage{{Name: aws.String(acm.ExtendedKeyUsageNameNone), OID: aws.String("")}}
	}
}

// applyLeafSummary fills the ListCertificates summary fields ACM reports for
// an imported certificate, derived on read as in applyLeafDetail.
func applyLeafSummary(sum *acm.CertificateSummary, rec *CertRecord) {
	if certTypeOrDefault(rec) != certTypeImported {
		return
	}
	sum.CreatedAt = timePtr(importedCreatedAt(rec))
	leaf := parseStoredLeaf(rec)
	if leaf == nil {
		return
	}
	if sans := importedSANs(leaf); len(sans) > 0 {
		sum.SubjectAlternativeNameSummaries = aws.StringSlice(sans[:min(len(sans), maxSANSummaries)])
		sum.HasAdditionalSubjectAlternativeNames = aws.Bool(len(sans) > maxSANSummaries)
	}
	if leaf.KeyUsage == 0 {
		sum.KeyUsages = aws.StringSlice([]string{acm.KeyUsageNameAny})
	}
	if len(leaf.ExtKeyUsage)+len(leaf.UnknownExtKeyUsage) == 0 {
		sum.ExtendedKeyUsages = aws.StringSlice([]string{acm.ExtendedKeyUsageNameNone})
	}
}
