package handlers_acm

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsAuditSerial is the serial of the certificate imported into real ACM in
// the response audit; awsAuditSerialText is how ACM rendered it.
const (
	awsAuditCN         = "awsdiff.example.com"
	awsAuditSerialHex  = "6c42f03e00b6ee54c0740448f8cad7939787c61c"
	awsAuditSerialText = "6c:42:f0:3e:00:b6:ee:54:c0:74:04:48:f8:ca:d7:93:97:87:c6:1c"
)

// genAuditShapedCert builds a certificate shaped like the one the audit
// imported into real ACM (openssl req -x509 -newkey rsa:2048 -subj /CN=...):
// RSA-2048, SHA-256 with RSA, CN only, no SAN, key-usage or EKU extension.
func genAuditShapedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serial, ok := new(big.Int).SetString(awsAuditSerialHex, 16)
	require.True(t, ok)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: awsAuditCN},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

func importCert(t *testing.T, svc *ACMServiceImpl, certPEM, keyPEM []byte) string {
	t.Helper()
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: certPEM, PrivateKey: keyPEM}, testAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.CertificateArn)
}

func describeCert(t *testing.T, svc *ACMServiceImpl, certArn string) *acm.CertificateDetail {
	t.Helper()
	out, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{CertificateArn: aws.String(certArn)}, testAccountID)
	require.NoError(t, err)
	return out.Certificate
}

// wireJSON encodes v as the gateway does and decodes it generically, so
// assertions see the members a client receives rather than the Go struct.
func wireJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	body, err := jsonutil.BuildJSON(v)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

// Expected values are copied from real ACM's DescribeCertificate response for
// the audit's certificate. Export is absent: aws-sdk-go v1 cannot carry it.
func TestDescribeCertificate_ImportedMatchesAWSAuditResponse(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genAuditShapedCert(t)
	d := describeCert(t, svc, importCert(t, svc, certPEM, keyPEM))

	assert.Equal(t, []string{awsAuditCN}, aws.StringValueSlice(d.SubjectAlternativeNames),
		"ACM lists the CN as the only SAN when the certificate has no SAN extension")
	assert.Equal(t, awsAuditSerialText, aws.StringValue(d.Serial))
	assert.Equal(t, awsAuditCN, aws.StringValue(d.Issuer))
	assert.Equal(t, "CN="+awsAuditCN, aws.StringValue(d.Subject))
	assert.Equal(t, "SHA256WITHRSA", aws.StringValue(d.SignatureAlgorithm))
	require.NotNil(t, d.CreatedAt)
	require.NotNil(t, d.ImportedAt)
	assert.True(t, d.CreatedAt.Equal(*d.ImportedAt), "CreatedAt equals ImportedAt on a first import")

	cert := wireJSON(t, &acm.DescribeCertificateOutput{Certificate: d})["Certificate"].(map[string]any)
	assert.Equal(t, []any{map[string]any{"DomainName": awsAuditCN}}, cert["DomainValidationOptions"],
		"an imported certificate's validation entries carry the domain name and nothing else")
	assert.Equal(t, []any{map[string]any{"Name": "ANY"}}, cert["KeyUsages"])
	assert.Equal(t, []any{map[string]any{"Name": "NONE", "OID": ""}}, cert["ExtendedKeyUsages"])
	assert.Equal(t, map[string]any{"CertificateTransparencyLoggingPreference": "DISABLED"}, cert["Options"])
}

func TestListCertificates_ImportedSummaryMatchesAWSAuditResponse(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genAuditShapedCert(t)
	importCert(t, svc, certPEM, keyPEM)

	list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	sums := wireJSON(t, list)["CertificateSummaryList"].([]any)
	require.Len(t, sums, 1)
	s := sums[0].(map[string]any)

	assert.Equal(t, []any{awsAuditCN}, s["SubjectAlternativeNameSummaries"])
	assert.Equal(t, false, s["HasAdditionalSubjectAlternativeNames"])
	assert.Equal(t, []any{"ANY"}, s["KeyUsages"])
	assert.Equal(t, []any{"NONE"}, s["ExtendedKeyUsages"])
	assert.Equal(t, "INELIGIBLE", s["RenewalEligibility"])
	require.Contains(t, s, "CreatedAt")
	assert.Equal(t, s["ImportedAt"], s["CreatedAt"])
}

func TestImportCertificate_ReimportKeepsCreatedAtAndMovesImportedAt(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genAuditShapedCert(t)
	certArn := importCert(t, svc, c1, k1)
	first := describeCert(t, svc, certArn)

	c2, k2 := genAuditShapedCert(t)
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		CertificateArn: aws.String(certArn), Certificate: c2, PrivateKey: k2,
	}, testAccountID)
	require.NoError(t, err)
	second := describeCert(t, svc, certArn)

	assert.True(t, second.CreatedAt.Equal(*first.CreatedAt), "re-import must not move CreatedAt")
	assert.True(t, second.ImportedAt.After(*first.ImportedAt), "re-import records the new import time")
}

// A record stored before these fields existed holds the old Serial and Issuer
// spellings, no SANs and no CreatedAt; it must still describe as AWS does.
func TestDescribeCertificate_LegacyImportedRecordDerivedFromPEM(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genAuditShapedCert(t)
	certArn := svc.mintCertificateArn(testAccountID)
	importedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, svc.store.PutCert(context.Background(), &CertRecord{
		CertificateArn: certArn,
		AccountID:      testAccountID,
		Certificate:    string(certPEM),
		PrivateKey:     string(keyPEM),
		DomainName:     awsAuditCN,
		Serial:         awsAuditSerialHex,
		Subject:        "CN=" + awsAuditCN,
		Issuer:         "CN=" + awsAuditCN,
		KeyAlgorithm:   "RSA_2048",
		ImportedAt:     importedAt,
	}))

	d := describeCert(t, svc, certArn)
	assert.Equal(t, awsAuditSerialText, aws.StringValue(d.Serial))
	assert.Equal(t, awsAuditCN, aws.StringValue(d.Issuer))
	assert.Equal(t, []string{awsAuditCN}, aws.StringValueSlice(d.SubjectAlternativeNames))
	require.NotNil(t, d.CreatedAt)
	assert.True(t, d.CreatedAt.Equal(importedAt), "a legacy record's CreatedAt falls back to ImportedAt")
	require.Len(t, d.KeyUsages, 1)
	require.Len(t, d.ExtendedKeyUsages, 1)
}

// What ACM shows for these certificates has not been observed, so none of
// the audit's defaults may be applied to them.
func TestDescribeCertificate_UnobservedCertificateShapesKeepPriorBehaviour(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genCert(t, "cn.example.com", "san-a.example.com", "san-b.example.com")
	certArn := importCert(t, svc, certPEM, keyPEM)
	d := describeCert(t, svc, certArn)

	assert.Equal(t, []string{"san-a.example.com", "san-b.example.com"}, aws.StringValueSlice(d.SubjectAlternativeNames),
		"whether ACM adds a CN missing from the SANs is unverified")
	assert.Nil(t, d.KeyUsages, "ACM's names for a present key-usage extension are unverified")
	assert.Nil(t, d.ExtendedKeyUsages, "ACM's names for a present EKU extension are unverified")
	assert.Nil(t, d.SignatureAlgorithm, "ACM's spelling for an ECDSA signature is unverified")
	require.Len(t, d.DomainValidationOptions, 2)

	list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, list.CertificateSummaryList, 1)
	assert.Nil(t, list.CertificateSummaryList[0].KeyUsages)
	assert.Nil(t, list.CertificateSummaryList[0].ExtendedKeyUsages)
}

func TestListCertificates_SANSummariesCappedAtOneHundred(t *testing.T) {
	svc := setupACMService(t)
	names := make([]string, maxSANSummaries+1)
	for i := range names {
		names[i] = fmt.Sprintf("n%03d.example.com", i)
	}
	certPEM, keyPEM := genCert(t, names[0], names...)
	importCert(t, svc, certPEM, keyPEM)

	list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	s := list.CertificateSummaryList[0]
	assert.Len(t, s.SubjectAlternativeNameSummaries, maxSANSummaries)
	assert.True(t, aws.BoolValue(s.HasAdditionalSubjectAlternativeNames))
}

// The tenant CA's DN carries an O as well as a CN, a case ACM was not observed
// on, so its leaves keep the full issuer DN; the serial format still applies.
func TestRequestCertificate_PrivateCA_SerialColonFormattedIssuerDNKept(t *testing.T) {
	svc := setupACMService(t)
	dir := t.TempDir()
	ca, err := LoadOrCreateTenantCA(filepath.Join(dir, "tenant-ca.pem"), filepath.Join(dir, "tenant-ca.key"), []string{"real.example.com"})
	require.NoError(t, err)
	svc.TenantCA = ca

	out, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{DomainName: aws.String("leaf.real.example.com")}, testAccountID)
	require.NoError(t, err)
	d := describeCert(t, svc, aws.StringValue(out.CertificateArn))

	rec, err := svc.store.GetCertMetadata(context.Background(), aws.StringValue(out.CertificateArn))
	require.NoError(t, err)
	leaf, err := parseLeaf([]byte(rec.Certificate))
	require.NoError(t, err)
	assert.Equal(t, formatSerial(leaf.SerialNumber), aws.StringValue(d.Serial))
	assert.Regexp(t, `^[0-9a-f]{2}(:[0-9a-f]{2})*$`, aws.StringValue(d.Serial))
	assert.Equal(t, leaf.Issuer.String(), aws.StringValue(d.Issuer))
	assert.Nil(t, d.Options, "a requested certificate's Options differ from an import's and are not set here")
}

func TestFormatSerial(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{1, "01"},
		{0x0a00ff, "0a:00:ff"},
	} {
		assert.Equal(t, tc.want, formatSerial(big.NewInt(tc.in)))
	}
}
