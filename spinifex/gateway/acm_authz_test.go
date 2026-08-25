//test:in-package — drives ACM_Request through the gateway's unexported test
// helpers (setupACMRequest, policyMockIAMService) and auth context keys.

package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_acm "github.com/mulgadc/spinifex/spinifex/gateway/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func acmCertARN(id string) string {
	return "arn:aws:acm:" + authzRegion + ":" + authzAccountID + ":certificate/" + id
}

// dispatchACM drives the gateway with no NATS connection. A permitted request
// therefore fails at the NATS-availability guard rather than on authorization,
// which is what proves the policy check ran ahead of the certificate existing.
func dispatchACM(t *testing.T, gw *GatewayConfig, action, body string) error {
	t.Helper()
	return gw.ACM_Request(httptest.NewRecorder(), setupACMRequest("CertificateManager."+action, body))
}

// TestACMRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production certificate; before the resolver the fence was inert and
// DeleteCertificate against it was permitted with nothing logged.
func TestACMRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "acm:*", "*"),
		statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertDenied(t, dispatchACM(t, gw, "DeleteCertificate", `{"CertificateArn":"`+acmCertARN("prod-1111")+`"}`))
	assertPermitted(t, dispatchACM(t, gw, "DeleteCertificate", `{"CertificateArn":"`+acmCertARN("dev-2222")+`"}`))
}

// TestACMRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
// The sibling is denied with AccessDenied and not a ResourceNotFound, which is
// what proves the gate runs ahead of existence.
func TestACMRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "acm:DescribeCertificate", acmCertARN("dev-2222")),
	)

	assertPermitted(t, dispatchACM(t, gw, "DescribeCertificate", `{"CertificateArn":"`+acmCertARN("dev-2222")+`"}`))
	assertDenied(t, dispatchACM(t, gw, "DescribeCertificate", `{"CertificateArn":"`+acmCertARN("prod-1111")+`"}`))
}

// A caller-supplied region or account in the ARN would let a request slide out
// from under a Deny scoped to the real one.
func TestACMRequest_CallerSuppliedAnchorIsIgnored(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "acm:*", "*"),
		statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertDenied(t, dispatchACM(t, gw, "DeleteCertificate",
		`{"CertificateArn":"arn:aws:acm:us-east-1:999999999999:certificate/prod-1111"}`))
}

// ImportCertificate creates unless it names a certificate to replace, so a grant
// on the type covers the create and a fence on one certificate covers only the
// re-import that names it.
func TestACMRequest_ImportResolvesBothShapes(t *testing.T) {
	create := scopedPolicyGateway(
		statement("Allow", "acm:ImportCertificate", acmCertARN("*")),
	)
	assertPermitted(t, dispatchACM(t, create, "ImportCertificate", `{"Certificate":"pem"}`))

	fenced := scopedPolicyGateway(
		statement("Allow", "acm:*", "*"),
		statement("Deny", "acm:ImportCertificate", acmCertARN("prod-1111")),
	)
	assertDenied(t, dispatchACM(t, fenced, "ImportCertificate",
		`{"CertificateArn":"`+acmCertARN("prod-1111")+`","Certificate":"pem"}`))
	assertPermitted(t, dispatchACM(t, fenced, "ImportCertificate", `{"Certificate":"pem"}`))
}

// RequestCertificate has no id at gate time, so it resolves the type.
func TestACMRequest_RequestCertificateResolvesTheType(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "acm:RequestCertificate", acmCertARN("*")),
	)
	assertPermitted(t, dispatchACM(t, gw, "RequestCertificate", `{"DomainName":"example.com"}`))

	scoped := scopedPolicyGateway(
		statement("Allow", "acm:RequestCertificate", acmCertARN("prod-1111")),
	)
	assertDenied(t, dispatchACM(t, scoped, "RequestCertificate", `{"DomainName":"example.com"}`))
}

// ListCertificates is account-level, so a grant scoped to one certificate does
// not reach it.
func TestACMRequest_ListIsAccountLevel(t *testing.T) {
	scoped := scopedPolicyGateway(
		statement("Allow", "acm:ListCertificates", acmCertARN("dev-2222")),
	)
	assertDenied(t, dispatchACM(t, scoped, "ListCertificates", `{}`))

	wide := scopedPolicyGateway(statement("Allow", "acm:ListCertificates", "*"))
	assertPermitted(t, dispatchACM(t, wide, "ListCertificates", `{}`))
}

// The property the per-service rollout rests on: every policy that works today
// carries Resource "*", and passing a real ARN cannot turn it into a denial.
func TestACMRequest_WildcardPolicyStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "acm:*", "*"))

	for _, action := range gateway_acm.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			assertPermitted(t, dispatchACM(t, gw, action, `{"CertificateArn":"`+acmCertARN("aaaa-1111")+`"}`))
		})
	}
}

// An absent identifier authorizes account-wide, so the handler still reports its
// own validation fault rather than the gate converting it into a denial.
func TestACMRequest_AbsentIdentifierStaysTheHandlersFault(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "acm:*", "*"),
		statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertPermitted(t, dispatchACM(t, gw, "DeleteCertificate", `{}`))
}

// A body the gate cannot parse authorizes account-wide for the same reason.
func TestACMRequest_UnparseableBodyStaysTheHandlersFault(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "acm:*", "*"),
		statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertPermitted(t, dispatchACM(t, gw, "DeleteCertificate", "{not json"))
}

// A body spelling one field two ways is rejected rather than authorized against
// whichever spelling the case fold happened to keep.
func TestACMRequest_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "acm:*", "*"))

	// Repeated: the fold's disagreement is random, the rejection must not be.
	for range 50 {
		err := dispatchACM(t, gw, "DeleteCertificate",
			`{"CertificateArn":"`+acmCertARN("dev-2222")+`","certificateArn":"`+acmCertARN("prod-1111")+`"}`)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestACMRequest_OversizedBodyIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "acm:*", "*"))
	body := `{"CertificateArn":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchACM(t, gw, "DeleteCertificate", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The account-ID guard moved above the policy gate, which used to reach its own
// InternalError first. That is the code the caller has always seen.
func TestACMRequest_MissingAccountID(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "acm:*", "*"))
	req := setupACMRequest("CertificateManager.ListCertificates", `{}`)
	req = req.WithContext(context.WithValue(req.Context(), ctxAccountID, ""))

	err := gw.ACM_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
