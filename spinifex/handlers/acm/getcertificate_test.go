package handlers_acm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCertificate_ReturnsBodyAndChain(t *testing.T) {
	svc := setupACMService(t)
	leafPEM, keyPEM := genCert(t, "get.example.com")
	chainPEM, _ := genCert(t, "issuer.example.com")

	imported, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:      leafPEM,
		PrivateKey:       keyPEM,
		CertificateChain: chainPEM,
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.GetCertificate(context.Background(), &acm.GetCertificateInput{
		CertificateArn: imported.CertificateArn,
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, string(leafPEM), aws.StringValue(out.Certificate))
	assert.Equal(t, string(chainPEM), aws.StringValue(out.CertificateChain))
}

// A self-signed leaf has no chain; the field is left absent rather than an
// empty string a client would have to distinguish from a real chain.
func TestGetCertificate_OmitsAbsentChain(t *testing.T) {
	svc := setupACMService(t)
	leafPEM, keyPEM := genCert(t, "nochain.example.com")

	imported, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: leafPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.GetCertificate(context.Background(), &acm.GetCertificateInput{
		CertificateArn: imported.CertificateArn,
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, string(leafPEM), aws.StringValue(out.Certificate))
	assert.Nil(t, out.CertificateChain)
}

func TestGetCertificate_UnknownArnNotFound(t *testing.T) {
	svc := setupACMService(t)

	_, err := svc.GetCertificate(context.Background(), &acm.GetCertificateInput{
		CertificateArn: aws.String("arn:aws:acm:ap-southeast-2:000000000001:certificate/does-not-exist"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorResourceNotFound)
}

// Another account's certificate is indistinguishable from a missing one — the
// body is the material itself, so a cross-account read must not confirm the
// certificate even exists.
func TestGetCertificate_CrossAccountNotFound(t *testing.T) {
	svc := setupACMService(t)
	leafPEM, keyPEM := genCert(t, "other.example.com")

	imported, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: leafPEM,
		PrivateKey:  keyPEM,
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.GetCertificate(context.Background(), &acm.GetCertificateInput{
		CertificateArn: imported.CertificateArn,
	}, "000000000002")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorResourceNotFound)
}

// A requested certificate has a record before it has a body. That is retryable
// and distinct from not-found, so a client polling for issuance can tell them
// apart.
func TestGetCertificate_PendingIssuanceIsRequestInProgress(t *testing.T) {
	svc := setupACMService(t)
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/pending-1"
	require.NoError(t, svc.store.PutCert(t.Context(), &CertRecord{
		CertificateArn: arn,
		AccountID:      testAccountID,
		DomainName:     "pending.example.com",
		Status:         acm.CertificateStatusPendingValidation,
	}))

	_, err := svc.GetCertificate(context.Background(), &acm.GetCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorACMRequestInProgress)
}

func TestGetCertificate_EmptyArnRejected(t *testing.T) {
	svc := setupACMService(t)

	_, err := svc.GetCertificate(context.Background(), &acm.GetCertificateInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorACMInvalidArn)
}

// putUndecryptableCert writes a record whose stored PrivateKey is neither valid
// ciphertext nor a PEM key, bypassing PutCert (which would encrypt it). Any read
// path that decrypts fails on this record, which is what makes it a detector:
// a path that succeeds provably never touched the key.
func putUndecryptableCert(t *testing.T, store *Store, arn, domain string) {
	t.Helper()
	data, err := json.Marshal(&CertRecord{
		CertificateArn: arn,
		AccountID:      testAccountID,
		DomainName:     domain,
		Certificate:    "-----BEGIN CERTIFICATE-----\nbody\n-----END CERTIFICATE-----\n",
		PrivateKey:     "not-pem-and-not-ciphertext",
		Status:         acm.CertificateStatusIssued,
	})
	require.NoError(t, err)
	_, err = rawKV(t, store).Put(t.Context(), certKey(arn), data)
	require.NoError(t, err)
}

// Pins that the summary paths never decrypt key material, by consequence rather
// than instrumentation: an undecryptable key is fatal to a decrypting read, so a
// list that still returns the certificate cannot have decrypted it.
func TestListPath_NeverTouchesKeyMaterial(t *testing.T) {
	svc := setupACMService(t)
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/nokeyread-1"
	putUndecryptableCert(t, svc.store, arn, "nokeyread.example.com")

	// Control: the decrypting read does fail on this record, so the assertions
	// below are testing the absence of a decrypt and not a benign key.
	_, err := svc.store.GetCert(t.Context(), arn)
	require.Error(t, err)

	list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, list.CertificateSummaryList, 1)
	assert.Equal(t, arn, aws.StringValue(list.CertificateSummaryList[0].CertificateArn))

	// Same for the single-record metadata reads layered on GetCertMetadata.
	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "nokeyread.example.com", aws.StringValue(desc.Certificate.DomainName))

	body, err := svc.GetCertificate(context.Background(), &acm.GetCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Contains(t, aws.StringValue(body.Certificate), "BEGIN CERTIFICATE")
}

// The metadata reads clear PrivateKey rather than handing back ciphertext, so a
// caller cannot mistake ciphertext for usable key material in a field that is
// plaintext PEM everywhere else.
func TestMetadataReads_ClearPrivateKey(t *testing.T) {
	store := setupACMStore(t)
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/cleared-1"
	_, keyPEM := genCert(t, "cleared.example.com")
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{
		CertificateArn: arn,
		AccountID:      testAccountID,
		DomainName:     "cleared.example.com",
		PrivateKey:     string(keyPEM),
	}))

	one, err := store.GetCertMetadata(t.Context(), arn)
	require.NoError(t, err)
	require.NotNil(t, one)
	assert.Empty(t, one.PrivateKey)

	recs, err := store.ListCertMetadata(t.Context(), testAccountID)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Empty(t, recs[0].PrivateKey)

	// The key is still there and still readable through the decrypting read —
	// the metadata reads withhold it, they do not lose it.
	full, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, string(keyPEM), full.PrivateKey)
}

func TestGetCertMetadata_AbsentReturnsNil(t *testing.T) {
	store := setupACMStore(t)

	rec, err := store.GetCertMetadata(t.Context(), "arn:aws:acm:ap-southeast-2:000000000001:certificate/missing")
	require.NoError(t, err)
	assert.Nil(t, rec)
}
