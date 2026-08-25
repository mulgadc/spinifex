//test:in-package — drives ECS_Request through the gateway's unexported test
// helpers (setupECSRequest, policyMockIAMService) and auth context keys.

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchECS drives the gateway with no NATS connection. A permitted request
// therefore reaches the handler and fails there, which is what proves the policy
// check ran ahead of the resource existing at all.
func dispatchECS(t *testing.T, gw *GatewayConfig, action, body string) error {
	t.Helper()
	req := setupECSRequest(gateway_ecs.TargetPrefix+"."+action, body)
	return gw.ECS_Request(httptest.NewRecorder(), req)
}

// assertReachedHandler asserts the policy gate passed. The request then fails on
// the absent NATS connection or on its 501 stub rather than on authorization, so
// a denial cannot hide behind a not-found.
func assertReachedHandler(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorAccessDenied, err.Error())
}

func ecsResourceARN(resource string) string {
	return "arn:aws:ecs:" + authzRegion + ":" + authzAccountID + ":" + resource
}

// TestECSRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production service; before the resolver the fence was inert and
// DeleteService against it was permitted with nothing logged.
func TestECSRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecs:*", "*"),
		statement("Deny", "ecs:DeleteService", "arn:aws:ecs:*:*:service/prod/web"),
	)

	assertDenied(t, dispatchECS(t, gw, "DeleteService", `{"cluster":"prod","service":"web"}`))
	assertReachedHandler(t, dispatchECS(t, gw, "DeleteService", `{"cluster":"prod","service":"api"}`))
	assertReachedHandler(t, dispatchECS(t, gw, "DeleteService", `{"cluster":"dev","service":"web"}`))
}

// TestECSRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestECSRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecs:UpdateCluster", "arn:aws:ecs:*:*:cluster/dev"),
	)

	err := dispatchECS(t, gw, "UpdateCluster", `{"cluster":"dev"}`)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
	assertDenied(t, dispatchECS(t, gw, "UpdateCluster", `{"cluster":"prod"}`))
}

// An omitted cluster is the default cluster, both at the gate and in the
// handler, so a fence on it fires.
func TestECSRequest_OmittedClusterIsFencedAsDefault(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecs:*", "*"),
		statement("Deny", "ecs:UpdateCluster", ecsResourceARN("cluster/default")),
	)

	assertDenied(t, dispatchECS(t, gw, "UpdateCluster", `{}`))
	assertDenied(t, dispatchECS(t, gw, "UpdateCluster", `{"cluster":"default"}`))
	assertReachedHandler(t, dispatchECS(t, gw, "UpdateCluster", `{"cluster":"prod"}`))
}

// A body the gate cannot parse authorizes account-wide, and the handler still
// reports its own validation fault.
func TestECSRequest_UnparseableBodyStaysTheHandlersFault(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecs:*", "*"),
		statement("Deny", "ecs:UpdateCluster", ecsResourceARN("cluster/prod")),
	)

	err := dispatchECS(t, gw, "UpdateCluster", "{not json")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// A body spelling one field two ways is the bypass a case-folded lookup opens:
// the handler reads the last spelling in document order, the gate's map cannot,
// so the request is rejected rather than authorized against the wrong cluster.
func TestECSRequest_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecs:*", "*"),
		statement("Deny", "ecs:DeleteCluster", ecsResourceARN("cluster/prod")),
	)

	// Repeated: the fold's disagreement is random, the rejection must not be.
	for range 50 {
		err := dispatchECS(t, gw, "DeleteCluster", `{"cluster":"dev","Cluster":"prod"}`)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

// A describe naming no cluster describes the default one in the handler, so a
// fence on the default cluster fires against an empty list too.
func TestECSRequest_OmittedClusterListIsFencedAsDefault(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ecs:*", "*"),
		statement("Deny", "ecs:DescribeClusters", ecsResourceARN("cluster/default")),
	)

	assertDenied(t, dispatchECS(t, gw, "DescribeClusters", `{}`))
	assertDenied(t, dispatchECS(t, gw, "DescribeClusters", `{"clusters":[]}`))
	assertReachedHandler(t, dispatchECS(t, gw, "DescribeClusters", `{"clusters":["prod"]}`))
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestECSRequest_OversizedBodyIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ecs:*", "*"))
	body := `{"cluster":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchECS(t, gw, "UpdateCluster", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestECSRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ecs:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/", nil).
		WithContext(context.WithValue(context.Background(), ctxAccountID, authzAccountID)))
	body := []byte(`{"cluster":"prod","service":"web","serviceName":"web","task":"t-1",` +
		`"containerInstance":"ci-1","taskDefinition":"app:1","name":"cp","capacityProvider":"cp",` +
		`"clusterName":"prod","resourceArn":"` + ecsResourceARN("cluster/prod") + `"}`)

	for _, action := range gateway_ecs.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			resources, err := gateway_ecs.ResourceARNs(action, authzRegion, authzAccountID, body)
			require.NoError(t, err)
			assert.NoError(t, gw.checkPolicyResources(req, "ecs", action, resources))
		})
	}
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestECSRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ecs:*", "*"))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Amz-Target", gateway_ecs.TargetPrefix+".ListClusters")
	req = withTestIdentity(req.WithContext(context.WithValue(req.Context(), ctxService, "ecs")))

	err := gw.ECS_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
