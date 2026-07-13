package gateway_bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubResolver reports a resolvable credential (with a fixed key) only for
// vendors in ok.
type stubResolver struct {
	ok map[string]bool
}

func (s stubResolver) Resolve(_ context.Context, _, vendor string) (string, bool, error) {
	if !s.ok[vendor] {
		return "", false, nil
	}
	return "stub-key", true, nil
}

func modelIDs(out *bedrock.ListFoundationModelsOutput) []string {
	ids := make([]string, 0, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		ids = append(ids, *m.ModelId)
	}
	return ids
}

func TestListFoundationModels_SelfHostAlwaysIncluded(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), "meta.llama3-70b-instruct-v1:0")
}

func TestListFoundationModels_ProviderIncludedWhenResolvable(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), "anthropic.claude-3-5-sonnet-20240620-v1:0")
}

func TestListFoundationModels_ProviderExcludedWhenUnresolvable(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), "anthropic.claude-3-5-sonnet-20240620-v1:0")
}

func TestGetFoundationModel_KnownModel(t *testing.T) {
	out, err := GetFoundationModel(context.Background(), "000000000001", "meta.llama3-70b-instruct-v1:0")
	require.NoError(t, err)
	require.NotNil(t, out.ModelDetails)
	assert.Equal(t, "meta.llama3-70b-instruct-v1:0", *out.ModelDetails.ModelId)
}

func TestGetFoundationModel_UnknownModelReturnsNotFound(t *testing.T) {
	_, err := GetFoundationModel(context.Background(), "000000000001", "does-not-exist")
	require.Error(t, err)
	assert.Equal(t, "ResourceNotFoundException", err.Error())
}
