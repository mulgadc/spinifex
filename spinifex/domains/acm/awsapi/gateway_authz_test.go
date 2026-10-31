package awsapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	. "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acmRequest builds the request an ACM JSON 1.1 client sends.
func acmRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	return req
}

// dispatchACM drives the registration with no NATS connection. A permitted request
// therefore fails at the NATS-availability guard rather than on authorization,
// which is what proves the policy check ran ahead of the certificate existing.
func dispatchACM(t *testing.T, h *gwtest.Harness, action, body string) error {
	t.Helper()
	return h.Dispatch(NewRegistration(Deps{}), ServiceName, httptest.NewRecorder(), acmRequest("CertificateManager."+action, body))
}

// assertDenied matches on the resolved code, not the message: a policy denial
// names the principal, action and resource ahead of the code.
func assertDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
}

// assertPermitted asserts the policy gate passed. The request then fails on the
// absent NATS connection rather than on authorization, so a denial cannot hide
// behind a not-found.
func assertPermitted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestACMRequest_MissingTarget(t *testing.T) {
	err := gwtest.AllowAll().Dispatch(NewRegistration(Deps{}), ServiceName, httptest.NewRecorder(), acmRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestACMRequest_UnknownAction(t *testing.T) {
	err := dispatchACM(t, gwtest.AllowAll(), "BogusAction", "{}")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A known action with no NATS connection passes routing + policy and fails at
// the NATS-availability guard.
func TestACMRequest_KnownActionNoNATS(t *testing.T) {
	err := dispatchACM(t, gwtest.AllowAll(), "ListCertificates", "{}")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func acmCertARN(id string) string {
	return "arn:aws:acm:" + gwtest.Region + ":" + gwtest.AccountID + ":certificate/" + id
}

// TestACMRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production certificate; before the resolver the fence was inert and
// DeleteCertificate against it was permitted with nothing logged.
func TestACMRequest_ScopedDenyFires(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:*", "*"),
		gwtest.Statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertDenied(t, dispatchACM(t, gw, "DeleteCertificate", `{"CertificateArn":"`+acmCertARN("prod-1111")+`"}`))
	assertPermitted(t, dispatchACM(t, gw, "DeleteCertificate", `{"CertificateArn":"`+acmCertARN("dev-2222")+`"}`))
}

// TestACMRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
// The sibling is denied with AccessDenied and not a ResourceNotFound, which is
// what proves the gate runs ahead of existence.
func TestACMRequest_ScopedAllowGrants(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:DescribeCertificate", acmCertARN("dev-2222")),
	)

	assertPermitted(t, dispatchACM(t, gw, "DescribeCertificate", `{"CertificateArn":"`+acmCertARN("dev-2222")+`"}`))
	assertDenied(t, dispatchACM(t, gw, "DescribeCertificate", `{"CertificateArn":"`+acmCertARN("prod-1111")+`"}`))
}

// A caller-supplied region or account in the ARN would let a request slide out
// from under a Deny scoped to the real one.
func TestACMRequest_CallerSuppliedAnchorIsIgnored(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:*", "*"),
		gwtest.Statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertDenied(t, dispatchACM(t, gw, "DeleteCertificate",
		`{"CertificateArn":"arn:aws:acm:us-east-1:999999999999:certificate/prod-1111"}`))
}

// ImportCertificate creates unless it names a certificate to replace, so a grant
// on the type covers the create and a fence on one certificate covers only the
// re-import that names it.
func TestACMRequest_ImportResolvesBothShapes(t *testing.T) {
	create := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:ImportCertificate", acmCertARN("*")),
	)
	assertPermitted(t, dispatchACM(t, create, "ImportCertificate", `{"Certificate":"pem"}`))

	fenced := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:*", "*"),
		gwtest.Statement("Deny", "acm:ImportCertificate", acmCertARN("prod-1111")),
	)
	assertDenied(t, dispatchACM(t, fenced, "ImportCertificate",
		`{"CertificateArn":"`+acmCertARN("prod-1111")+`","Certificate":"pem"}`))
	assertPermitted(t, dispatchACM(t, fenced, "ImportCertificate", `{"Certificate":"pem"}`))
}

// RequestCertificate has no id at gate time, so it resolves the type.
func TestACMRequest_RequestCertificateResolvesTheType(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:RequestCertificate", acmCertARN("*")),
	)
	assertPermitted(t, dispatchACM(t, gw, "RequestCertificate", `{"DomainName":"example.com"}`))

	scoped := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:RequestCertificate", acmCertARN("prod-1111")),
	)
	assertDenied(t, dispatchACM(t, scoped, "RequestCertificate", `{"DomainName":"example.com"}`))
}

// A grant scoped to one certificate does not become a licence to mint new ones
// by naming that certificate in a field RequestCertificate discards.
func TestACMRequest_RequestCertificateIgnoresACertificateArnInTheBody(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:*", acmCertARN("team-a-1")),
	)

	assertDenied(t, dispatchACM(t, gw, "RequestCertificate",
		`{"DomainName":"evil.example","CertificateArn":"`+acmCertARN("team-a-1")+`"}`))
}

// ListCertificates is account-level, so a grant scoped to one certificate does
// not reach it.
func TestACMRequest_ListIsAccountLevel(t *testing.T) {
	scoped := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:ListCertificates", acmCertARN("dev-2222")),
	)
	assertDenied(t, dispatchACM(t, scoped, "ListCertificates", `{}`))

	wide := gwtest.Scoped(gwtest.Statement("Allow", "acm:ListCertificates", "*"))
	assertPermitted(t, dispatchACM(t, wide, "ListCertificates", `{}`))
}

// The property the per-service rollout rests on: every policy that works today
// carries Resource "*", and passing a real ARN cannot turn it into a denial.
func TestACMRequest_WildcardPolicyStillPermitsEveryAction(t *testing.T) {
	gw := gwtest.Scoped(gwtest.Statement("Allow", "acm:*", "*"))

	for _, action := range ScopedActions() {
		t.Run(action, func(t *testing.T) {
			assertPermitted(t, dispatchACM(t, gw, action, `{"CertificateArn":"`+acmCertARN("aaaa-1111")+`"}`))
		})
	}
}

// An absent identifier authorizes account-wide, so the handler still reports its
// own validation fault rather than the gate converting it into a denial.
func TestACMRequest_AbsentIdentifierStaysTheHandlersFault(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:*", "*"),
		gwtest.Statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertPermitted(t, dispatchACM(t, gw, "DeleteCertificate", `{}`))
}

// A body the gate cannot parse authorizes account-wide for the same reason.
func TestACMRequest_UnparseableBodyStaysTheHandlersFault(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "acm:*", "*"),
		gwtest.Statement("Deny", "acm:DeleteCertificate", acmCertARN("prod-1111")),
	)

	assertPermitted(t, dispatchACM(t, gw, "DeleteCertificate", "{not json"))
}

// A body spelling one field two ways is rejected rather than authorized against
// whichever spelling the case fold happened to keep.
func TestACMRequest_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	gw := gwtest.Scoped(gwtest.Statement("Allow", "acm:*", "*"))

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
	gw := gwtest.Scoped(gwtest.Statement("Allow", "acm:*", "*"))
	body := `{"CertificateArn":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchACM(t, gw, "DeleteCertificate", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The account-ID guard moved above the policy gate, which used to reach its own
// InternalError first. That is the code the caller has always seen.
func TestACMRequest_MissingAccountID(t *testing.T) {
	gw := gwtest.Scoped(gwtest.Statement("Allow", "acm:*", "*"))
	gw.AccountID = ""

	err := dispatchACM(t, gw, "ListCertificates", `{}`)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
