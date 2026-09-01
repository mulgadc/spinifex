//test:in-package — drives Tagging_Request through the gateway's unexported test
// helpers (setupTaggingRequest, policyMockIAMService).

package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_tagging "github.com/mulgadc/spinifex/spinifex/gateway/tagging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchTagging drives the gateway with no NATS connection. A permitted
// request therefore fails at the NATS-availability guard rather than on
// authorization, which is what proves the policy check ran ahead of the handler.
func dispatchTagging(t *testing.T, gw *GatewayConfig, action, body string) error {
	t.Helper()
	return gw.Tagging_Request(httptest.NewRecorder(), setupTaggingRequest("ResourceGroupsTaggingAPI_20170126."+action, body))
}

// Every tagging action is account-level, so a grant scoped to a resource ARN
// does not reach one. This is the whole enforcement surface the service admits:
// AWS defines no resource types for the Resource Groups Tagging API, so there is
// no ARN a scoped Deny could name.
func TestTaggingRequest_ActionsAreAccountLevel(t *testing.T) {
	scoped := scopedPolicyGateway(
		statement("Allow", "tagging:GetResources", "arn:aws:ec2:"+authzRegion+":"+authzAccountID+":instance/i-dev"),
	)
	assertDenied(t, dispatchTagging(t, scoped, "GetResources", `{}`))

	wide := scopedPolicyGateway(statement("Allow", "tagging:GetResources", "*"))
	assertPermitted(t, dispatchTagging(t, wide, "GetResources", `{}`))
}

// The property the per-service rollout rests on: every policy that works today
// carries Resource "*", and resolving the scope table cannot turn one into a
// denial.
func TestTaggingRequest_WildcardPolicyStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "tagging:*", "*"))

	for _, action := range gateway_tagging.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			assertPermitted(t, dispatchTagging(t, gw, action, `{}`))
		})
	}
}

// The action half of the evaluator is unchanged: a grant on one action does not
// carry to another the dispatcher serves.
func TestTaggingRequest_UnrelatedActionGrantDenies(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ec2:DescribeTags", "*"))

	assertDenied(t, dispatchTagging(t, gw, "GetResources", `{}`))
}

// A body past the signed path's cap cannot be used to exhaust memory: it is
// rejected before the handler reads it.
func TestTaggingRequest_OversizedBodyIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "tagging:*", "*"))
	body := `{"PaginationToken":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchTagging(t, gw, "GetResources", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}
