package acm

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allKeyTypes lists every certificate whatever its key type.
func allKeyTypes() *acm.ListCertificatesInput {
	return &acm.ListCertificatesInput{Includes: &acm.Filters{KeyTypes: aws.StringSlice(acm.KeyAlgorithm_Values())}}
}

func listTestArn(name string) string {
	return "arn:aws:acm:ap-southeast-2:" + testAccountID + ":certificate/" + name
}

// putListRecord stores a metadata-only record, so a test controls the ARN,
// key algorithm, status and creation time.
func putListRecord(t *testing.T, svc *ACMServiceImpl, name, alg, status string, createdAt time.Time) {
	t.Helper()
	require.NoError(t, svc.store.PutCert(t.Context(), &CertRecord{
		CertificateArn: listTestArn(name),
		AccountID:      testAccountID,
		DomainName:     name + ".example.com",
		KeyAlgorithm:   alg,
		Status:         status,
		CreatedAt:      createdAt,
	}))
}

func listedNames(t *testing.T, out *acm.ListCertificatesOutput) []string {
	t.Helper()
	names := make([]string, 0, len(out.CertificateSummaryList))
	for _, s := range out.CertificateSummaryList {
		names = append(names, aws.StringValue(s.DomainName))
	}
	return names
}

// The EC certificate sorts inside the first page, so a page cut before the
// default key-type filter would leave that page short.
func TestListCertificates_PageWalkFollowsFiltering(t *testing.T) {
	t.Parallel()
	svc := setupACMService(t)
	putListRecord(t, svc, "a", "RSA-2048", "", time.Time{})
	putListRecord(t, svc, "b", "EC-prime256v1", "", time.Time{})
	putListRecord(t, svc, "c", "RSA-1024", "", time.Time{})
	putListRecord(t, svc, "d", "RSA-2048", "", time.Time{})
	putListRecord(t, svc, "e", "RSA-2048", "", time.Time{})

	var pages [][]string
	in := &acm.ListCertificatesInput{MaxItems: aws.Int64(2)}
	for range 5 {
		out, err := svc.ListCertificates(context.Background(), in, testAccountID)
		require.NoError(t, err)
		pages = append(pages, listedNames(t, out))
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	assert.Equal(t, [][]string{
		{"a.example.com", "c.example.com"},
		{"d.example.com", "e.example.com"},
	}, pages)
}

// Without Includes.keyTypes AWS lists only RSA_1024 and RSA_2048. A requested
// certificate has no key yet and counts as RSA_2048, the request default.
func TestListCertificates_DefaultKeyTypesAreRSA1024And2048(t *testing.T) {
	t.Parallel()
	svc := setupACMService(t)
	putListRecord(t, svc, "rsa1024", "RSA-1024", "", time.Time{})
	putListRecord(t, svc, "rsa2048", "RSA-2048", "", time.Time{})
	putListRecord(t, svc, "rsa4096", "RSA-4096", "", time.Time{})
	putListRecord(t, svc, "ec", "EC-prime256v1", "", time.Time{})
	putListRecord(t, svc, "pending", "", acm.CertificateStatusPendingValidation, time.Time{})

	out, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"rsa1024.example.com", "rsa2048.example.com", "pending.example.com"}, listedNames(t, out))

	out, err = svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{
		Includes: &acm.Filters{KeyTypes: aws.StringSlice([]string{acm.KeyAlgorithmEcPrime256v1, acm.KeyAlgorithmRsa4096})},
	}, testAccountID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ec.example.com", "rsa4096.example.com"}, listedNames(t, out))
}

func TestListCertificates_StatusFilter(t *testing.T) {
	t.Parallel()
	svc := setupACMService(t)
	putListRecord(t, svc, "issued", "RSA-2048", acm.CertificateStatusIssued, time.Time{})
	putListRecord(t, svc, "pending", "RSA-2048", acm.CertificateStatusPendingValidation, time.Time{})
	putListRecord(t, svc, "failed", "RSA-2048", acm.CertificateStatusFailed, time.Time{})

	out, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{
		CertificateStatuses: aws.StringSlice([]string{acm.CertificateStatusPendingValidation, acm.CertificateStatusFailed}),
	}, testAccountID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"pending.example.com", "failed.example.com"}, listedNames(t, out))
}

// genCert carries DIGITAL_SIGNATURE and server auth; genRSACert carries neither.
func TestListCertificates_KeyUsageFilters(t *testing.T) {
	t.Parallel()
	svc := setupACMService(t)
	c1, k1 := genCert(t, "usage.example.com", "usage.example.com")
	c2, k2 := genRSACert(t, "bare.example.com")
	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c1, PrivateKey: k1}, testAccountID)
	require.NoError(t, err)
	_, err = svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c2, PrivateKey: k2}, testAccountID)
	require.NoError(t, err)

	in := allKeyTypes()
	in.Includes.KeyUsage = aws.StringSlice([]string{acm.KeyUsageNameDigitalSignature})
	out, err := svc.ListCertificates(context.Background(), in, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"usage.example.com"}, listedNames(t, out))

	in = allKeyTypes()
	in.Includes.ExtendedKeyUsage = aws.StringSlice([]string{acm.ExtendedKeyUsageNameNone})
	out, err = svc.ListCertificates(context.Background(), in, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"bare.example.com"}, listedNames(t, out))
}

func TestListCertificates_SortByCreatedAt(t *testing.T) {
	t.Parallel()
	svc := setupACMService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	putListRecord(t, svc, "a-newest", "RSA-2048", "", base.Add(2*time.Hour))
	putListRecord(t, svc, "b-oldest", "RSA-2048", "", base)
	putListRecord(t, svc, "c-middle", "RSA-2048", "", base.Add(time.Hour))

	out, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{
		SortBy: aws.String(acm.SortByCreatedAt), SortOrder: aws.String(acm.SortOrderAscending),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"b-oldest.example.com", "c-middle.example.com", "a-newest.example.com"}, listedNames(t, out))

	out, err = svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{
		SortBy: aws.String(acm.SortByCreatedAt), SortOrder: aws.String(acm.SortOrderDescending), MaxItems: aws.Int64(2),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-newest.example.com", "c-middle.example.com"}, listedNames(t, out))
	require.NotNil(t, out.NextToken)
}

func TestListCertificates_RefusesBadPageInput(t *testing.T) {
	t.Parallel()
	svc := setupACMService(t)
	cases := []struct {
		name string
		in   *acm.ListCertificatesInput
		code string
	}{
		{"zero MaxItems", &acm.ListCertificatesInput{MaxItems: aws.Int64(0)}, awserrors.ErrorValidationException},
		{"MaxItems above 1000", &acm.ListCertificatesInput{MaxItems: aws.Int64(1001)}, awserrors.ErrorValidationException},
		{"token we did not issue", &acm.ListCertificatesInput{NextToken: aws.String("not-a-token")}, awserrors.ErrorACMInvalidArgs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.ListCertificates(context.Background(), tc.in, testAccountID)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "err = %v", err)
			assert.Equal(t, tc.code, code)
		})
	}
}
