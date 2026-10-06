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
	return strings.ReplaceAll(fmt.Sprintf("% x", serial.Bytes()), " ", ":")
}

// signatureAlgorithms are ACM's spellings of the leaf's signature algorithm.
var signatureAlgorithms = map[x509.SignatureAlgorithm]string{
	x509.SHA256WithRSA:   "SHA256WITHRSA",
	x509.SHA384WithRSA:   "SHA384WITHRSA",
	x509.SHA512WithRSA:   "SHA512WITHRSA",
	x509.ECDSAWithSHA256: "SHA256WITHECDSA",
	x509.ECDSAWithSHA384: "SHA384WITHECDSA",
	x509.ECDSAWithSHA512: "SHA512WITHECDSA",
}

// keyUsageNames is in x509.KeyUsage bit order, which is the order ACM lists them.
var keyUsageNames = []string{
	acm.KeyUsageNameDigitalSignature, acm.KeyUsageNameNonRepudiation, acm.KeyUsageNameKeyEncipherment,
	acm.KeyUsageNameDataEncipherment, acm.KeyUsageNameKeyAgreement, acm.KeyUsageNameCertificateSigning,
	acm.KeyUsageNameCrlSigning, acm.KeyUsageNameEncipherOnly, acm.KeyUsageNameDecipherOnly,
}

var extKeyUsageNames = map[x509.ExtKeyUsage]struct{ name, oid string }{
	x509.ExtKeyUsageAny:             {acm.ExtendedKeyUsageNameAny, "2.5.29.37.0"},
	x509.ExtKeyUsageServerAuth:      {acm.ExtendedKeyUsageNameTlsWebServerAuthentication, "1.3.6.1.5.5.7.3.1"},
	x509.ExtKeyUsageClientAuth:      {acm.ExtendedKeyUsageNameTlsWebClientAuthentication, "1.3.6.1.5.5.7.3.2"},
	x509.ExtKeyUsageCodeSigning:     {acm.ExtendedKeyUsageNameCodeSigning, "1.3.6.1.5.5.7.3.3"},
	x509.ExtKeyUsageEmailProtection: {acm.ExtendedKeyUsageNameEmailProtection, "1.3.6.1.5.5.7.3.4"},
	x509.ExtKeyUsageIPSECEndSystem:  {acm.ExtendedKeyUsageNameIpsecEndSystem, "1.3.6.1.5.5.7.3.5"},
	x509.ExtKeyUsageIPSECTunnel:     {acm.ExtendedKeyUsageNameIpsecTunnel, "1.3.6.1.5.5.7.3.6"},
	x509.ExtKeyUsageIPSECUser:       {acm.ExtendedKeyUsageNameIpsecUser, "1.3.6.1.5.5.7.3.7"},
	x509.ExtKeyUsageTimeStamping:    {acm.ExtendedKeyUsageNameTimeStamping, "1.3.6.1.5.5.7.3.8"},
	x509.ExtKeyUsageOCSPSigning:     {acm.ExtendedKeyUsageNameOcspSigning, "1.3.6.1.5.5.7.3.9"},
}

// issuerName is how ACM names the issuer: its O when it has one, else its CN.
func issuerName(leaf *x509.Certificate) string {
	if len(leaf.Issuer.Organization) > 0 {
		return leaf.Issuer.Organization[0]
	}
	if leaf.Issuer.CommonName != "" {
		return leaf.Issuer.CommonName
	}
	return leaf.Issuer.String()
}

// importedSANs is what ACM reports for an imported certificate: the CN, then
// any DNS SANs not already listed.
func importedSANs(leaf *x509.Certificate) []string {
	return uniqueDomains(leafDomain(leaf), leaf.DNSNames)
}

// keyUsages returns ACM's key-usage names, or ANY when the extension is absent.
func keyUsages(leaf *x509.Certificate) []string {
	if leaf.KeyUsage == 0 {
		return []string{acm.KeyUsageNameAny}
	}
	var names []string
	for i, name := range keyUsageNames {
		if leaf.KeyUsage&(1<<i) != 0 {
			names = append(names, name)
		}
	}
	return names
}

// extKeyUsages returns ACM's extended-key-usage names and OIDs, or NONE with an
// empty OID when the extension is absent. Unrecognised OIDs are CUSTOM.
func extKeyUsages(leaf *x509.Certificate) (names, oids []string) {
	if len(leaf.ExtKeyUsage)+len(leaf.UnknownExtKeyUsage) == 0 {
		return []string{acm.ExtendedKeyUsageNameNone}, []string{""}
	}
	for _, u := range leaf.ExtKeyUsage {
		if e, ok := extKeyUsageNames[u]; ok {
			names, oids = append(names, e.name), append(oids, e.oid)
		}
	}
	for _, oid := range leaf.UnknownExtKeyUsage {
		names, oids = append(names, acm.ExtendedKeyUsageNameCustom), append(oids, oid.String())
	}
	return names, oids
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
		if alg, ok := signatureAlgorithms[leaf.SignatureAlgorithm]; ok {
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
	if sans := importedSANs(leaf); len(sans) > 0 {
		detail.SubjectAlternativeNames = aws.StringSlice(sans)
		detail.DomainValidationOptions = make([]*acm.DomainValidation, len(sans))
		for i, d := range sans {
			detail.DomainValidationOptions[i] = &acm.DomainValidation{DomainName: aws.String(d)}
		}
	}
	for _, name := range keyUsages(leaf) {
		detail.KeyUsages = append(detail.KeyUsages, &acm.KeyUsage{Name: aws.String(name)})
	}
	names, oids := extKeyUsages(leaf)
	for i := range names {
		detail.ExtendedKeyUsages = append(detail.ExtendedKeyUsages, &acm.ExtendedKeyUsage{Name: aws.String(names[i]), OID: aws.String(oids[i])})
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
	sum.KeyUsages = aws.StringSlice(keyUsages(leaf))
	names, _ := extKeyUsages(leaf)
	sum.ExtendedKeyUsages = aws.StringSlice(names)
}
