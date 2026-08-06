package gateway

import (
	"context"
	"testing"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupBedrockAction_ResolvesKnownRoutes(t *testing.T) {
	cases := []struct {
		method, path string
		wantAction   string
		wantParams   []string
	}{
		{"GET", "/foundation-models", "ListFoundationModels", nil},
		{"GET", "/foundation-models/anthropic.claude-3-5-sonnet-20240620-v1:0", "GetFoundationModel", []string{"anthropic.claude-3-5-sonnet-20240620-v1:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			action, params, handler, ok := lookupBedrockAction(tc.method, tc.path)
			require.True(t, ok, "expected route to match for %s %s", tc.method, tc.path)
			require.NotNil(t, handler)
			assert.Equal(t, tc.wantAction, action)
			assert.Equal(t, tc.wantParams, params)
		})
	}
}

func TestLookupBedrockAction_UnknownReturnsFalse(t *testing.T) {
	_, _, handler, ok := lookupBedrockAction("DELETE", "/foundation-models")
	assert.False(t, ok)
	assert.Nil(t, handler)
}

func TestLookupBedrockRuntimeAction_ResolvesConverse(t *testing.T) {
	action, params, handler, ok := lookupBedrockRuntimeAction("POST", "/model/meta.llama3-70b-instruct-v1:0/converse")
	require.True(t, ok)
	require.NotNil(t, handler)
	assert.Equal(t, "Converse", action)
	assert.Equal(t, []string{"meta.llama3-70b-instruct-v1:0"}, params)
}

func TestLookupBedrockRuntimeAction_UnknownReturnsFalse(t *testing.T) {
	_, _, handler, ok := lookupBedrockRuntimeAction("GET", "/model/foo/converse")
	assert.False(t, ok)
	assert.Nil(t, handler)
}

// stubEndpointResolver stands in for the registry-backed resolver, so the
// preference test needs no NATS connection.
type stubEndpointResolver struct{ baseURL string }

var _ gateway_bedrock.EndpointResolver = stubEndpointResolver{}

func (s stubEndpointResolver) Endpoint(context.Context, string) (string, bool, error) {
	return s.baseURL, s.baseURL != "", nil
}

// TestBedrockEndpointResolver_PrefersDynamic pins the wiring the whole
// dynamic-resolution change hangs off: with a registry resolver configured the
// gateway must use it, since it is the only path that can request a launch.
func TestBedrockEndpointResolver_PrefersDynamic(t *testing.T) {
	gw := &GatewayConfig{
		BedrockEndpoints:        map[string]string{"m": "http://static:8000"},
		BedrockEndpointResolver: stubEndpointResolver{baseURL: "http://dynamic:8000"},
	}
	baseURL, ok, err := gw.bedrockEndpointResolver().Endpoint(context.Background(), "m")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "http://dynamic:8000", baseURL)
}

// TestBedrockEndpointResolver_FallsBackToStatic keeps every gateway built
// without a registry resolver (including the tests) on its pinned map.
func TestBedrockEndpointResolver_FallsBackToStatic(t *testing.T) {
	gw := &GatewayConfig{BedrockEndpoints: map[string]string{"m": "http://static:8000"}}
	baseURL, ok, err := gw.bedrockEndpointResolver().Endpoint(context.Background(), "m")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "http://static:8000", baseURL)
}
