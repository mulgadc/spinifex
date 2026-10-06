package handlers_acm

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genRSACert returns a self-signed RSA leaf with only a CN, the shape of the
// certificate the AWS conformance audit imported.
func genRSACert(t *testing.T, cn string, bits int) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// describeAndListKeyAlgorithm returns the KeyAlgorithm that DescribeCertificate
// and the ListCertificates summary report for certArn.
func describeAndListKeyAlgorithm(t *testing.T, svc *ACMServiceImpl, certArn string) (described, listed string) {
	t.Helper()
	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)

	list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	for _, s := range list.CertificateSummaryList {
		if aws.StringValue(s.CertificateArn) == certArn {
			return aws.StringValue(desc.Certificate.KeyAlgorithm), aws.StringValue(s.KeyAlgorithm)
		}
	}
	t.Fatalf("ListCertificates did not return %s", certArn)
	return "", ""
}

// AWS returns RSA-2048 with a hyphen from both Describe and List, although the
// API enum (and the ListCertificates keyTypes filter) spells it RSA_2048.
func TestImportCertificate_RSAKeyAlgorithmIsHyphenated(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genRSACert(t, "rsa.example.com", 2048)

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)

	described, listed := describeAndListKeyAlgorithm(t, svc, aws.StringValue(out.CertificateArn))
	assert.Equal(t, "RSA-2048", described)
	assert.Equal(t, "RSA-2048", listed)

	// The read path normalises, so check the stored spelling separately.
	raw, _, err := svc.store.store.Get(t.Context(), certKey(aws.StringValue(out.CertificateArn)))
	require.NoError(t, err)
	assert.Equal(t, "RSA-2048", raw.KeyAlgorithm)
}

// Records written before the spelling fix hold RSA_<bits> or EC_<Go curve>;
// they must read back in the AWS spelling without a migration.
func TestKeyAlgorithm_StoredUnderscoreSpellingReadsBackHyphenated(t *testing.T) {
	cases := []struct {
		stored, want string
	}{
		{stored: "RSA_2048", want: "RSA-2048"},
		{stored: "RSA_4096", want: "RSA-4096"},
		{stored: "RSA-3072", want: "RSA-3072"},
		{stored: "EC_P-256", want: "EC-prime256v1"},
	}
	svc := setupACMService(t)
	for i, tc := range cases {
		t.Run(tc.stored, func(t *testing.T) {
			certArn := "arn:aws:acm:ap-southeast-2:" + testAccountID + ":certificate/legacy-" + string(rune('a'+i))
			require.NoError(t, svc.store.PutCert(t.Context(), &CertRecord{
				CertificateArn: certArn,
				AccountID:      testAccountID,
				DomainName:     "legacy.example.com",
				KeyAlgorithm:   tc.stored,
			}))

			described, listed := describeAndListKeyAlgorithm(t, svc, certArn)
			assert.Equal(t, tc.want, described)
			assert.Equal(t, tc.want, listed)
		})
	}
}
