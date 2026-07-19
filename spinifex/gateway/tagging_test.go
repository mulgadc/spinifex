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

func setupTaggingRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	ctx := context.WithValue(req.Context(), ctxService, "tagging")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	return req.WithContext(ctx)
}

func TestTaggingActionFromTarget(t *testing.T) {
	assert.Equal(t, "GetResources", taggingActionFromTarget("ResourceGroupsTaggingAPI_20170126.GetResources"))
	assert.Equal(t, "GetResources", taggingActionFromTarget("GetResources"))
	assert.Empty(t, taggingActionFromTarget(""))
}

func TestTaggingActionsMap_AllActionsRegistered(t *testing.T) {
	expected := []string{
		"GetResources",
	}
	for _, action := range expected {
		_, ok := taggingActions[action]
		assert.True(t, ok, "action %q should be registered in taggingActions", action)
	}
	assert.Len(t, taggingActions, len(expected))
}

func TestTaggingRequest_MissingTarget(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.Tagging_Request(w, setupTaggingRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestTaggingRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.Tagging_Request(w, setupTaggingRequest("ResourceGroupsTaggingAPI_20170126.TagResources", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A known action with no NATS connection passes routing + policy and fails at
// the NATS-availability guard.
func TestTaggingRequest_KnownActionNoNATS(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	err := gw.Tagging_Request(w, setupTaggingRequest("ResourceGroupsTaggingAPI_20170126.GetResources", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
