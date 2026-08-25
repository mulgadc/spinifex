package gateway_acm_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_acm "github.com/mulgadc/spinifex/spinifex/gateway/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRegion    = "ap-southeast-2"
	testAccountID = "123456789012"
)

func certARN(id string) string {
	return "arn:aws:acm:" + testRegion + ":" + testAccountID + ":certificate/" + id
}

func resolve(t *testing.T, action, body string) []string {
	t.Helper()
	resources, err := gateway_acm.ResourceARNs(action, testRegion, testAccountID, []byte(body))
	require.NoError(t, err)
	return resources
}

// The ARN handed to the evaluator names the certificate the handler resolves.
// ACM has one resource class, so this is the whole fidelity surface.
func TestResourceARNsFidelity(t *testing.T) {
	tests := []struct {
		action string
		body   string
		want   []string
	}{
		{"DescribeCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},
		{"GetCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},
		{"DeleteCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},
		{"ListTagsForCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},
		{"AddTagsToCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},
		{"RemoveTagsFromCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},

		// AWS JSON 1.1 spells the field lower-camel; the SDK struct spells it
		// upper-camel. Both name the same certificate.
		{"DescribeCertificate", `{"certificateArn":"` + certARN("aaaa-1111") + `"}`, []string{certARN("aaaa-1111")}},

		// Creates resolve the type, which is what AWS evaluates them against.
		{"RequestCertificate", `{"DomainName":"example.com"}`, []string{certARN("*")}},
		{"ImportCertificate", `{"Certificate":"pem"}`, []string{certARN("*")}},

		// A re-import names the certificate it replaces.
		{"ImportCertificate", `{"CertificateArn":"` + certARN("aaaa-1111") + `","Certificate":"pem"}`, []string{certARN("aaaa-1111")}},

		// Account-level: AWS documents no resource type for the list.
		{"ListCertificates", `{}`, []string{"*"}},
	}

	for _, tt := range tests {
		t.Run(tt.action+" "+tt.body, func(t *testing.T) {
			assert.Equal(t, tt.want, resolve(t, tt.action, tt.body))
		})
	}
}

// A caller-supplied region or account would let a request slide out from under
// a Deny scoped to the real one. The handler only ever acts on a certificate in
// the caller's own account, so the ARN is rebuilt under it.
func TestResourceARNsIgnoresTheCallersAnchor(t *testing.T) {
	foreign := `{"CertificateArn":"arn:aws:acm:us-east-1:999999999999:certificate/aaaa-1111"}`

	assert.Equal(t, []string{certARN("aaaa-1111")}, resolve(t, "DeleteCertificate", foreign))
}

// An absent identifier authorizes account-wide, so a malformed request stays the
// handler's validation fault rather than becoming an authorization failure.
func TestResourceARNsAbsentIdentifier(t *testing.T) {
	for _, body := range []string{`{}`, ``, `{"CertificateArn":""}`, `{"CertificateArn":"not-an-arn"}`} {
		assert.Equal(t, []string{"*"}, resolve(t, "DeleteCertificate", body), "body %q", body)
	}
}

// A body the gate cannot parse resolves account-wide for the same reason.
func TestResourceARNsUnparseableBody(t *testing.T) {
	assert.Equal(t, []string{"*"}, resolve(t, "DeleteCertificate", "{not json"))
}

// A body carrying two spellings of one field is rejected: the gate and the
// handler would otherwise name different certificates.
func TestResourceARNsAmbiguousBody(t *testing.T) {
	_, err := gateway_acm.ResourceARNs("DeleteCertificate", testRegion, testAccountID,
		[]byte(`{"CertificateArn":"`+certARN("aaaa-1111")+`","certificateArn":"`+certARN("bbbb-2222")+`"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// The id is a value, not a pattern. An id that is literally "*" builds an ARN
// ending "/*", which neither matches a scoped Deny nor widens a grant, and an id
// containing "/" keeps it rather than being truncated.
func TestResourceARNsIdentifierIsAValue(t *testing.T) {
	assert.Equal(t, []string{certARN("*")},
		resolve(t, "DeleteCertificate", `{"CertificateArn":"`+certARN("*")+`"}`))
	assert.Equal(t, []string{certARN("aaaa-1111/admin")},
		resolve(t, "DeleteCertificate", `{"CertificateArn":"`+certARN("aaaa-1111/admin")+`"}`))
}

// An action absent from the dispatch table cannot reach the resolver, but if one
// ever did it fails closed rather than authorizing account-wide.
func TestResourceARNsUnknownAction(t *testing.T) {
	_, err := gateway_acm.ResourceARNs("BogusAction", testRegion, testAccountID, []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// Without a region or an account there is no ARN to build, and every action
// falls back to the account-wide resource.
func TestResourceARNsWithoutAnAnchor(t *testing.T) {
	for _, action := range gateway_acm.ScopedActions() {
		resources, err := gateway_acm.ResourceARNs(action, "", "", []byte(`{"CertificateArn":"`+certARN("aaaa-1111")+`"}`))
		require.NoError(t, err)
		assert.Equal(t, []string{"*"}, resources, "action %q", action)
	}
}

func TestHasScope(t *testing.T) {
	assert.True(t, gateway_acm.HasScope("DeleteCertificate"))
	assert.False(t, gateway_acm.HasScope("BogusAction"))
	assert.Len(t, gateway_acm.ScopedActions(), 9)
}
