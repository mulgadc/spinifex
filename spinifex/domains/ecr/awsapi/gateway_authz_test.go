package awsapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ecrRequest builds the request an ECR JSON 1.1 client sends.
func ecrRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	return req
}

// dispatchECR drives the registration at an action whose handler is the 501 stub, so
// a permitted request fails there rather than on authorization.
func dispatchECR(t *testing.T, h *gwtest.Harness, action, body string) error {
	t.Helper()
	return h.Dispatch(awsapi.NewRegistration(awsapi.Deps{}), awsapi.ServiceName, httptest.NewRecorder(), ecrRequest(awsapi.TargetPrefix+"."+action, body))
}

func assertECRPermitted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// TestECRRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production repository; before the resolver the fence was inert.
func TestECRRequest_ScopedDenyFires(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:*", "*"),
		gwtest.Statement("Deny", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/prod"),
	)

	gwtest.AssertDenied(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"prod"}`))
	assertECRPermitted(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"dev"}`))
}

// TestECRRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestECRRequest_ScopedAllowGrants(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/dev"),
	)

	assertECRPermitted(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"dev"}`))
	gwtest.AssertDenied(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"prod"}`))
}

// A body spelling one field two ways is rejected rather than authorized against
// whichever spelling the case fold happened to keep.
func TestECRRequest_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:*", "*"),
		gwtest.Statement("Deny", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/prod"),
	)

	// Repeated: the fold's disagreement is random, the rejection must not be.
	for range 50 {
		err := dispatchECR(t, gw, "BatchCheckLayerAvailability",
			`{"repositoryName":"dev","RepositoryName":"prod"}`)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorECRInvalidParameter, awserrors.ValidErrorCodeFromError(err))
	}
}

// DescribeRepositories naming no repository enumerates the whole registry, so a
// grant scoped to one repository does not reach it.
func TestECRRequest_OmittedRepositoryListEnumeratesTheRegistry(t *testing.T) {
	scoped := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:DescribeRepositories", "arn:aws:ecr:*:*:repository/dev"),
	)
	gwtest.AssertDenied(t, dispatchECR(t, scoped, "DescribeRepositories", `{}`))
	gwtest.AssertDenied(t, dispatchECR(t, scoped, "DescribeRepositories", `{"repositoryNames":[]}`))

	wide := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:DescribeRepositories", "arn:aws:ecr:*:*:repository/*"),
	)
	gwtest.AssertNotDenied(t, dispatchECR(t, wide, "DescribeRepositories", `{}`))
}

// A caller-supplied registryId would let a request slide out from under a Deny
// scoped to the real account, so the ARN is built under the caller's.
func TestECRRequest_RegistryIDInTheBodyIsIgnored(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:*", "*"),
		gwtest.Statement("Deny", "ecr:BatchCheckLayerAvailability",
			"arn:aws:ecr:"+gwtest.Region+":"+gwtest.AccountID+":repository/prod"),
	)

	gwtest.AssertDenied(t, dispatchECR(t, gw, "BatchCheckLayerAvailability",
		`{"registryId":"999999999999","repositoryName":"prod"}`))
}

// A body the gate cannot parse authorizes account-wide, and the handler still
// reports its own fault.
func TestECRRequest_UnparseableBodyStaysTheHandlersFault(t *testing.T) {
	gw := gwtest.Scoped(
		gwtest.Statement("Allow", "ecr:*", "*"),
		gwtest.Statement("Deny", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/prod"),
	)

	assertECRPermitted(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", "{not json"))
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestECRRequest_OversizedBodyIsRejected(t *testing.T) {
	gw := gwtest.Scoped(gwtest.Statement("Allow", "ecr:*", "*"))
	body := `{"repositoryName":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchECR(t, gw, "BatchCheckLayerAvailability", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestECRRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := gwtest.Scoped(gwtest.Statement("Allow", "ecr:*", "*"))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	caller := credential.Principal{
		AccountID:     gwtest.AccountID,
		Identity:      "alice",
		PrincipalType: stsdomain.PrincipalTypeUser,
		CallerARN:     "arn:aws:iam::" + gwtest.AccountID + ":user/alice",
	}
	body := []byte(`{"repositoryName":"app","repositoryNames":["app"],` +
		`"resourceArn":"arn:aws:ecr:` + gwtest.Region + `:` + gwtest.AccountID + `:repository/app"}`)

	for _, action := range awsapi.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			resources, err := awsapi.ResourceARNs(action, gwtest.Region, gwtest.AccountID, body)
			require.NoError(t, err)
			assert.NoError(t, pipeline.Authorize(gw.Policy, req, caller, true, "ecr", action, resources, nil))
		})
	}
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestECRRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := gwtest.Scoped(gwtest.Statement("Allow", "ecr:*", "*"))
	gw.AccountID = ""

	err := dispatchECR(t, gw, "ListRepositories", "{}")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
