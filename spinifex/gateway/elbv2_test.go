package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupELBv2Request(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "elasticloadbalancing")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	return req.WithContext(ctx)
}

func TestELBv2Request_MissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.ELBv2_Request(w, setupELBv2Request(""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestELBv2Request_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.ELBv2_Request(w, setupELBv2Request("Action=FakeAction"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestELBv2ActionsMap_AllActionsRegistered(t *testing.T) {
	expectedActions := []string{
		"CreateLoadBalancer",
		"DeleteLoadBalancer",
		"DescribeLoadBalancers",
		"CreateTargetGroup",
		"ModifyTargetGroup",
		"DeleteTargetGroup",
		"DescribeTargetGroups",
		"RegisterTargets",
		"DeregisterTargets",
		"DescribeTargetHealth",
		"CreateListener",
		"DeleteListener",
		"ModifyListener",
		"DescribeListeners",
		"DescribeTags",
		"AddTags",
		"RemoveTags",
		"SetSecurityGroups",
		"SetIpAddressType",
		"LBAgentHeartbeat",
		"GetLBConfig",
		"ModifyTargetGroupAttributes",
		"DescribeTargetGroupAttributes",
		"ModifyLoadBalancerAttributes",
		"DescribeLoadBalancerAttributes",
		"DescribeListenerAttributes",
		"ModifyListenerAttributes",
		"CreateRule",
		"ModifyRule",
		"DeleteRule",
		"DescribeRules",
		"SetRulePriorities",
	}

	for _, action := range expectedActions {
		_, ok := elbv2Actions[action]
		assert.True(t, ok, "action %q should be registered in elbv2Actions", action)
	}

	assert.Len(t, elbv2Actions, len(expectedActions), "elbv2Actions should have exactly %d actions", len(expectedActions))
}

func TestELBv2ActionsMap_UnknownActionNotRegistered(t *testing.T) {
	unknownActions := []string{
		"ModifyLoadBalancer",
		"RunInstances",
	}

	for _, action := range unknownActions {
		_, ok := elbv2Actions[action]
		assert.False(t, ok, "action %q should not be in elbv2Actions", action)
	}
}
