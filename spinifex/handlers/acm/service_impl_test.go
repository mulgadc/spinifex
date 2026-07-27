package handlers_acm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "000000000001"

func setupACMService(t *testing.T) *ACMServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := NewACMServiceImplWithNATS(t.Context(), nil, nc, masterKey)
	require.NoError(t, err)
	return svc
}

// genCert returns a self-signed leaf certificate + private key as PEM for the
// given common name.
func genCert(t *testing.T, cn string, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestImportCertificate_MintsArnAndParsesLeaf(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genCert(t, "example.com", "example.com", "www.example.com")

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.CertificateArn)
	assert.Contains(t, aws.StringValue(out.CertificateArn), "arn:aws:acm:")
	assert.Contains(t, aws.StringValue(out.CertificateArn), ":"+testAccountID+":certificate/")

	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{
		CertificateArn: out.CertificateArn,
	}, testAccountID)
	require.NoError(t, err)
	d := desc.Certificate
	assert.Equal(t, "example.com", aws.StringValue(d.DomainName))
	assert.Equal(t, certStatusIssued, aws.StringValue(d.Status))
	assert.Equal(t, certTypeImported, aws.StringValue(d.Type))
	assert.Equal(t, "EC_P-256", aws.StringValue(d.KeyAlgorithm))
	assert.ElementsMatch(t, []string{"example.com", "www.example.com"}, aws.StringValueSlice(d.SubjectAlternativeNames))
	require.NotNil(t, d.NotAfter)
}

func TestImportCertificate_MismatchedKeyRejected(t *testing.T) {
	svc := setupACMService(t)
	certPEM, _ := genCert(t, "a.example.com")
	_, otherKey := genCert(t, "b.example.com")

	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  otherKey,
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameter)
}

func TestImportCertificate_GarbagePEMRejected(t *testing.T) {
	svc := setupACMService(t)
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: []byte("not a pem"),
		PrivateKey:  []byte("nope"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameter)
}

func TestListCertificates_ScopedToAccount(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "one.example.com")
	c2, k2 := genCert(t, "two.example.com")

	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)
	_, err = svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c2, PrivateKey: k2}, "000000000002")
	require.NoError(t, err)

	list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, list.CertificateSummaryList, 1)
	assert.Equal(t, "one.example.com", aws.StringValue(list.CertificateSummaryList[0].DomainName))
}

func TestDescribeCertificate_CrossAccountHidden(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "secret.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{
		CertificateArn: out.CertificateArn,
	}, "000000000002")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorResourceNotFound)
}

func TestDeleteCertificate(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "del.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteCertificate(context.Background(), &acm.DeleteCertificateInput{CertificateArn: out.CertificateArn}, testAccountID)
	require.NoError(t, err)

	// Gone now.
	_, err = svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{CertificateArn: out.CertificateArn}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorResourceNotFound)

	// Deleting again → ResourceNotFound.
	_, err = svc.DeleteCertificate(context.Background(), &acm.DeleteCertificateInput{CertificateArn: out.CertificateArn}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorResourceNotFound)
}

func TestImportCertificate_ReimportPreservesInUseBy(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "inuse.example.com")

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)

	require.NoError(t, svc.store.AddInUseBy(context.Background(), certArn, "arn:lb/one"))

	// Re-import different material under the same ARN.
	c2, k2 := genCert(t, "inuse-rotated.example.com")
	_, err = svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c2,
		PrivateKey:     k2,
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{CertificateArn: aws.String(certArn)}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "inuse-rotated.example.com", aws.StringValue(desc.Certificate.DomainName), "material must be replaced")
	assert.Equal(t, []string{"arn:lb/one"}, aws.StringValueSlice(desc.Certificate.InUseBy), "InUseBy must survive re-import")
}

func TestImportCertificate_ReimportInvokesFanoutHook(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "hook.example.com")

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)
	require.NoError(t, svc.store.AddInUseBy(context.Background(), certArn, "arn:lb/one"))

	var calledWith string
	svc.CertMaterialUpdated = func(ctx context.Context, arn string) error {
		calledWith = arn
		return nil
	}

	c2, k2 := genCert(t, "hook-rotated.example.com")
	_, err = svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c2,
		PrivateKey:     k2,
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, certArn, calledWith, "re-import to an in-use ARN must invoke the fan-out hook")
}

func TestImportCertificate_ReimportZeroInUseBy_HookNotCalled(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "unused.example.com")

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)

	called := false
	svc.CertMaterialUpdated = func(ctx context.Context, arn string) error {
		called = true
		return nil
	}

	// Re-import with zero load balancers referencing the ARN — a no-op fan-out.
	c2, k2 := genCert(t, "unused-rotated.example.com")
	_, err = svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c2,
		PrivateKey:     k2,
		CertificateArn: aws.String(certArn),
	}, testAccountID)
	require.NoError(t, err)
	assert.False(t, called, "a certificate referenced by zero load balancers must not trigger a fan-out")
}

func TestImportCertificate_FreshImportDoesNotInvokeHook(t *testing.T) {
	svc := setupACMService(t)
	called := false
	svc.CertMaterialUpdated = func(ctx context.Context, arn string) error {
		called = true
		return nil
	}

	c1, k1 := genCert(t, "fresh.example.com")
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)
	assert.False(t, called, "a brand-new ARN cannot yet be in use by anything")
}

func TestImportCertificate_ReimportUnknownArnRejected(t *testing.T) {
	svc := setupACMService(t)
	c1, k1 := genCert(t, "re.example.com")
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c1,
		PrivateKey:     k1,
		CertificateArn: aws.String("arn:aws:acm:ap-southeast-2:000000000001:certificate/does-not-exist"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorResourceNotFound)
}

// TestImportCertificate_PrivateKeyEncryptedAtRest is the core regression for
// mulga-ubkoz: the raw bytes ImportCertificate lands in JetStream KV must not
// contain the plaintext PEM key, and reading it back through the Store must
// still recover the original plaintext.
func TestImportCertificate_PrivateKeyEncryptedAtRest(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genCert(t, "atrest.example.com")

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)

	entry, err := svc.store.kv.Get(context.Background(), certKey(aws.StringValue(out.CertificateArn)))
	require.NoError(t, err)

	var raw CertRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &raw))
	assert.NotEqual(t, string(keyPEM), raw.PrivateKey, "private key must not be stored as plaintext PEM")
	assert.NotContains(t, string(entry.Value()), "PRIVATE KEY", "raw KV bytes must not contain a PEM private key marker")

	got, err := svc.store.GetCert(context.Background(), aws.StringValue(out.CertificateArn))
	require.NoError(t, err)
	assert.Equal(t, string(keyPEM), got.PrivateKey, "GetCert must decrypt back to the original plaintext")
}

// TestDescribeCertificate_DoesNotLeakPrivateKey asserts the public
// CertificateDetail response can never carry key material, however the
// record is stored. Precedent: TestSensitiveDataNotLogged_MasterKey in
// handlers/iam/service_impl_test.go.
func TestDescribeCertificate_DoesNotLeakPrivateKey(t *testing.T) {
	svc := setupACMService(t)
	certPEM, keyPEM := genCert(t, "noleak.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{
		CertificateArn: out.CertificateArn,
	}, testAccountID)
	require.NoError(t, err)

	b, err := json.Marshal(desc)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "PRIVATE KEY", "DescribeCertificate response must never carry key material")
}

// TestSensitiveDataNotLogged_ACMPrivateKey asserts ImportCertificate and
// DescribeCertificate never place the raw private key PEM in log output.
// Precedent: TestSensitiveDataNotLogged_MasterKey in
// handlers/iam/service_impl_test.go.
func TestSensitiveDataNotLogged_ACMPrivateKey(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	})

	svc := setupACMService(t)
	certPEM, keyPEM := genCert(t, "quiet.example.com")

	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{
		CertificateArn: out.CertificateArn,
	}, testAccountID)
	require.NoError(t, err)

	assert.NotContains(t, buf.String(), string(keyPEM), "raw private key PEM must not appear in log output")
}
