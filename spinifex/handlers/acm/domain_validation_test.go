package handlers_acm

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsBadDomainMessage is AWS's RequestCertificate answer for DomainName "not a domain".
const awsBadDomainMessage = "1 validation error detected: Value of the input at 'domainName' failed to satisfy constraint: Member must satisfy regular expression pattern: (\\*\\.)?(((?!-)[A-Za-z0-9-]{0,62}[A-Za-z0-9])\\.)+((?!-)[A-Za-z0-9-]{1,62}[A-Za-z0-9])"

func TestValidDomainName(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	label64 := strings.Repeat("a", 64)
	cases := []struct {
		domain string
		want   bool
	}{
		{"example.com", true},
		{"www.example.com", true},
		{"*.example.com", true},
		{"a.io", true},
		{"x-1.Example-Corp.COM", true},
		{"1.2.3.4", false}, // final label must be at least 2 characters
		{"10.20.30.40", true},
		{label63 + ".com", true},
		{"example." + label63, true},
		{label64 + ".com", false},
		{"example." + label64, false},
		{"not a domain", false},
		{"localhost", false},
		{"*.com", false},
		{"*", false},
		{"example.c", false},
		{"-example.com", false},
		{"example-.com", false},
		{"example.-com", false},
		{"example.com-", false},
		{"example.com.", false},
		{".example.com", false},
		{"example..com", false},
		{"www.*.example.com", false},
		{"**.example.com", false},
		{"*example.com", false},
		{"under_score.example.com", false},
		{"bücher.example.com", false},
		{"", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, validDomainName(tc.domain), "domain %q", tc.domain)
	}
}

// Every validation mode must answer a malformed domain with AWS's
// ValidationException, before its own checks, and store nothing.
func TestRequestCertificate_MalformedDomainIsValidationExceptionInEveryMode(t *testing.T) {
	const bad = "not a domain"
	modes := map[string]func(*ACMServiceImpl){
		"no tenant CA": func(*ACMServiceImpl) {},
		"tenant CA": func(s *ACMServiceImpl) {
			s.TenantCA = &fakeCertAuthority{permitted: map[string]bool{bad: true}}
		},
		"dns provider": func(s *ACMServiceImpl) { s.dnsProviderConfigured = true },
		"northstar zone": func(s *ACMServiceImpl) {
			s.NorthstarHostsZone = func(string) bool { return true }
		},
	}
	for name, configure := range modes {
		t.Run(name, func(t *testing.T) {
			svc := setupACMService(t)
			configure(svc)

			_, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
				DomainName: aws.String(bad),
			}, testAccountID)
			require.Error(t, err)

			code, message, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorValidationException, code)
			assert.Equal(t, awsBadDomainMessage, message)

			list, err := svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
			require.NoError(t, err)
			assert.Empty(t, list.CertificateSummaryList)
		})
	}
}

// The domain check must not pre-empt the missing-CA answer for a valid domain.
func TestRequestCertificate_ValidDomainWithoutTenantCAStillResourceNotFound(t *testing.T) {
	svc := setupACMService(t)
	_, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("*.valid.example.com"),
	}, testAccountID)
	require.Error(t, err)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorResourceNotFound, code)
}
