package gateway

import (
	"testing"

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
