//test:in-package — drives ECR_Request through the gateway's unexported test
// helpers (setupECRRequest, policyMockIAMService) and auth context keys.

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchECR drives the gateway at an action whose handler is the 501 stub, so
// a permitted request fails there rather than on authorization.
func dispatchECR(t *testing.T, gw *GatewayConfig, action, body string) error {
	t.Helper()
	return gw.ECR_Request(httptest.NewRecorder(), setupECRRequest(gateway_ecrapi.TargetPrefix+"."+action, body))
}

func assertECRPermitted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// TestECRRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production repository; before the resolver the fence was inert.
func TestECRRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecr:*", "*"),
		statement("Deny", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/prod"),
	)

	assertDenied(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"prod"}`))
	assertECRPermitted(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"dev"}`))
}

// TestECRRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestECRRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/dev"),
	)

	assertECRPermitted(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"dev"}`))
	assertDenied(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", `{"repositoryName":"prod"}`))
}

// A body spelling one field two ways is rejected rather than authorized against
// whichever spelling the case fold happened to keep.
func TestECRRequest_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecr:*", "*"),
		statement("Deny", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/prod"),
	)

	// Repeated: the fold's disagreement is random, the rejection must not be.
	for range 50 {
		err := dispatchECR(t, gw, "BatchCheckLayerAvailability",
			`{"repositoryName":"dev","RepositoryName":"prod"}`)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

// DescribeRepositories naming no repository enumerates the whole registry, so a
// grant scoped to one repository does not reach it.
func TestECRRequest_OmittedRepositoryListEnumeratesTheRegistry(t *testing.T) {
	scoped := scopedPolicyGateway(
		statement("Allow", "ecr:DescribeRepositories", "arn:aws:ecr:*:*:repository/dev"),
	)
	assertDenied(t, dispatchECR(t, scoped, "DescribeRepositories", `{}`))
	assertDenied(t, dispatchECR(t, scoped, "DescribeRepositories", `{"repositoryNames":[]}`))

	wide := scopedPolicyGateway(
		statement("Allow", "ecr:DescribeRepositories", "arn:aws:ecr:*:*:repository/*"),
	)
	assertNotDenied(t, dispatchECR(t, wide, "DescribeRepositories", `{}`))
}

// A caller-supplied registryId would let a request slide out from under a Deny
// scoped to the real account, so the ARN is built under the caller's.
func TestECRRequest_RegistryIDInTheBodyIsIgnored(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecr:*", "*"),
		statement("Deny", "ecr:BatchCheckLayerAvailability",
			"arn:aws:ecr:"+authzRegion+":"+authzAccountID+":repository/prod"),
	)

	assertDenied(t, dispatchECR(t, gw, "BatchCheckLayerAvailability",
		`{"registryId":"999999999999","repositoryName":"prod"}`))
}

// A body the gate cannot parse authorizes account-wide, and the handler still
// reports its own fault.
func TestECRRequest_UnparseableBodyStaysTheHandlersFault(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecr:*", "*"),
		statement("Deny", "ecr:BatchCheckLayerAvailability", "arn:aws:ecr:*:*:repository/prod"),
	)

	assertECRPermitted(t, dispatchECR(t, gw, "BatchCheckLayerAvailability", "{not json"))
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestECRRequest_OversizedBodyIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ecr:*", "*"))
	body := `{"repositoryName":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchECR(t, gw, "BatchCheckLayerAvailability", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestECRRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ecr:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/", nil).
		WithContext(context.WithValue(context.Background(), ctxAccountID, authzAccountID)))
	body := []byte(`{"repositoryName":"app","repositoryNames":["app"],` +
		`"resourceArn":"arn:aws:ecr:` + authzRegion + `:` + authzAccountID + `:repository/app"}`)

	for _, action := range gateway_ecrapi.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			resources, err := gateway_ecrapi.ResourceARNs(action, authzRegion, authzAccountID, body)
			require.NoError(t, err)
			assert.NoError(t, gw.checkPolicyResources(req, "ecr", action, resources))
		})
	}
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestECRRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ecr:*", "*"))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Amz-Target", gateway_ecrapi.TargetPrefix+".ListRepositories")
	req = withTestIdentity(req.WithContext(context.WithValue(req.Context(), ctxService, "ecr")))

	err := gw.ECR_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
