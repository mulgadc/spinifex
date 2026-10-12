package handlers_acm

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func importTagged(t *testing.T, svc *ACMServiceImpl, tags ...*acm.Tag) string {
	t.Helper()
	certPEM, keyPEM := genCert(t, "tags.example.com", "tags.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate: certPEM,
		PrivateKey:  keyPEM,
		Tags:        tags,
	}, testAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.CertificateArn)
}

func TestListTagsForCertificate_ReturnsImportTags(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tags, 1)
	assert.Equal(t, "Name", aws.StringValue(out.Tags[0].Key))
	assert.Equal(t, "ingress", aws.StringValue(out.Tags[0].Value))
}

func TestListTagsForCertificate_NoTagsReturnsEmpty(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc)

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tags)
}

func TestAddTagsToCertificate_MergesAndPersists(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	_, err := svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{
		CertificateArn: aws.String(arn),
		Tags: []*acm.Tag{
			{Key: aws.String("env"), Value: aws.String("dev")},
			{Key: aws.String("Name"), Value: aws.String("ingress-2")},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	got := map[string]string{}
	for _, tg := range out.Tags {
		got[aws.StringValue(tg.Key)] = aws.StringValue(tg.Value)
	}
	assert.Equal(t, map[string]string{"Name": "ingress-2", "env": "dev"}, got)
}

func TestRemoveTagsFromCertificate_ByKeyAndByValue(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc,
		&acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")},
		&acm.Tag{Key: aws.String("env"), Value: aws.String("dev")},
		&acm.Tag{Key: aws.String("team"), Value: aws.String("infra")},
	)

	// Key-only removal drops "Name"; value-mismatch keeps "env"; exact match drops "team".
	_, err := svc.RemoveTagsFromCertificate(context.Background(), &acm.RemoveTagsFromCertificateInput{
		CertificateArn: aws.String(arn),
		Tags: []*acm.Tag{
			{Key: aws.String("Name")},
			{Key: aws.String("env"), Value: aws.String("prod")},
			{Key: aws.String("team"), Value: aws.String("infra")},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tags, 1)
	assert.Equal(t, "env", aws.StringValue(out.Tags[0].Key))
	assert.Equal(t, "dev", aws.StringValue(out.Tags[0].Value))
}

func TestTagOps_CrossAccountHidden(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	_, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, "000000000002")
	assert.Equal(t, awserrors.ErrorResourceNotFound, err.Error())
}

// Tag writes must change only the stored tag field: the encrypted key
// survives byte-for-byte and still decrypts, and the tag change takes effect.
func TestTagOps_PreserveStoredPrivateKey(t *testing.T) {
	cases := map[string]struct {
		write    func(svc *ACMServiceImpl, certArn string) error
		wantTags map[string]string
	}{
		"AddTagsToCertificate": {
			write: func(svc *ACMServiceImpl, certArn string) error {
				_, err := svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{
					CertificateArn: aws.String(certArn),
					Tags:           []*acm.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
				}, testAccountID)
				return err
			},
			wantTags: map[string]string{"Name": "ingress", "env": "dev"},
		},
		"RemoveTagsFromCertificate": {
			write: func(svc *ACMServiceImpl, certArn string) error {
				_, err := svc.RemoveTagsFromCertificate(context.Background(), &acm.RemoveTagsFromCertificateInput{
					CertificateArn: aws.String(certArn),
					Tags:           []*acm.Tag{{Key: aws.String("Name")}},
				}, testAccountID)
				return err
			},
			wantTags: map[string]string{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc := setupACMService(t)
			certArn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

			beforeRaw := rawStoredRecord(t, svc, certArn)
			require.NotEmpty(t, beforeRaw.PrivateKey, "import must store an encrypted key")
			beforeUsable, err := svc.store.GetCert(context.Background(), certArn)
			require.NoError(t, err)
			require.NotEmpty(t, beforeUsable.PrivateKey, "import must store a usable key")

			require.NoError(t, tc.write(svc, certArn))

			afterRaw := rawStoredRecord(t, svc, certArn)
			assert.Equal(t, beforeRaw.PrivateKey, afterRaw.PrivateKey, "a tag write must not touch the stored encrypted key")
			assert.Equal(t, beforeRaw.Certificate, afterRaw.Certificate, "the certificate body survives")

			afterUsable, err := svc.store.GetCert(context.Background(), certArn)
			require.NoError(t, err)
			assert.Equal(t, beforeUsable.PrivateKey, afterUsable.PrivateKey, "the key must still decrypt to the original plaintext")

			assert.Equal(t, tc.wantTags, listTagMap(t, svc, certArn), "the requested tag change must still take effect")
		})
	}
}

// rawStoredRecord reads the certificate record exactly as Store.PutCert wrote
// it, with PrivateKey still AES-256-GCM ciphertext, bypassing both GetCert's
// decrypt and GetCertMetadata's blank.
func rawStoredRecord(t *testing.T, svc *ACMServiceImpl, certArn string) *CertRecord {
	t.Helper()
	rec, _, err := svc.store.store.Get(context.Background(), certKey(certArn))
	require.NoError(t, err)
	return rec
}

func listTagMap(t *testing.T, svc *ACMServiceImpl, arn string) map[string]string {
	t.Helper()
	out, err := svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	got := map[string]string{}
	for _, tg := range out.Tags {
		got[aws.StringValue(tg.Key)] = aws.StringValue(tg.Value)
	}
	return got
}

// AWS answers a re-import carrying Tags with ValidationException and leaves
// the stored certificate, material and tags alike, untouched.
func TestImportCertificate_ReimportWithTagsRejected(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})

	c2, k2 := genCert(t, "rotated.example.com", "rotated.example.com")
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c2,
		PrivateKey:     k2,
		CertificateArn: aws.String(arn),
		Tags:           []*acm.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.Error(t, err)

	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorValidationException, code)
	assert.Equal(t, "Tagging is not permitted on re-import.", message)
	assert.Equal(t, 400, awserrors.ErrorLookup[code].HTTPCode)

	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{CertificateArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "tags.example.com", aws.StringValue(desc.Certificate.DomainName), "a refused re-import must not replace the material")
	assert.Equal(t, map[string]string{"Name": "ingress"}, listTagMap(t, svc, arn))
}

// A re-import without Tags keeps the tags already on the certificate rather
// than clearing them, as InUseBy is kept.
func TestImportCertificate_ReimportKeepsStoredTags(t *testing.T) {
	svc := setupACMService(t)
	arn := importTagged(t, svc, &acm.Tag{Key: aws.String("Name"), Value: aws.String("ingress")})
	_, err := svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{
		CertificateArn: aws.String(arn),
		Tags:           []*acm.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	}, testAccountID)
	require.NoError(t, err)

	c2, k2 := genCert(t, "rotated.example.com", "rotated.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c2,
		PrivateKey:     k2,
		CertificateArn: aws.String(arn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, arn, aws.StringValue(out.CertificateArn))

	desc, err := svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{CertificateArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "rotated.example.com", aws.StringValue(desc.Certificate.DomainName), "material must be replaced")
	assert.Equal(t, map[string]string{"Name": "ingress", "env": "dev"}, listTagMap(t, svc, arn))
}

// AWS resolves the ARN before refusing the tags, so an unknown ARN with tags is
// a ResourceNotFoundException, not the tagging ValidationException.
func TestImportCertificate_ReimportWithTagsUnknownArnIsNotFound(t *testing.T) {
	svc := setupACMService(t)
	c, k := genCert(t, "x.example.com", "x.example.com")
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{
		Certificate:    c,
		PrivateKey:     k,
		CertificateArn: aws.String(svc.mintCertificateArn(testAccountID)),
		Tags:           []*acm.Tag{{Key: aws.String("a"), Value: aws.String("b")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorResourceNotFound, err.Error())
}
