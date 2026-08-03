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

// stubWeightsResolver reports a resolvable weights snapshot only for model
// IDs in ok.
type stubWeightsResolver struct {
	ok map[string]bool
}

func (s stubWeightsResolver) Resolve(_ context.Context, modelID string) (string, bool, error) {
	if !s.ok[modelID] {
		return "", false, nil
	}
	return "snap-stub", true, nil
}

// withWeightsResolver installs r as the package-level weights resolver for
// the duration of the test, restoring the no-op default on cleanup —
// ListFoundationModels and GetFoundationModel read it via
// currentWeightsResolver rather than a parameter.
func withWeightsResolver(t *testing.T, r WeightsResolver) {
	t.Helper()
	SetWeightsResolver(r)
	t.Cleanup(func() { SetWeightsResolver(nil) })
}

func modelIDs(out *bedrock.ListFoundationModelsOutput) []string {
	ids := make([]string, 0, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		ids = append(ids, *m.ModelId)
	}
	return ids
}

func TestListFoundationModels_SelfHostIncludedWhenWeightsResolve(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{"meta.llama3-2-1b-instruct-v1:0": true}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), "meta.llama3-2-1b-instruct-v1:0")
}

func TestListFoundationModels_SelfHostExcludedWhenWeightsUnresolvable(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), "meta.llama3-2-1b-instruct-v1:0")
}

func TestListFoundationModels_ProviderIncludedWhenResolvable(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), "anthropic.claude-3-5-sonnet-20240620-v1:0")
}

func TestListFoundationModels_ProviderExcludedWhenUnresolvable(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), "anthropic.claude-3-5-sonnet-20240620-v1:0")
}

func TestGetFoundationModel_KnownModelWithResolvableWeights(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{"meta.llama3-2-1b-instruct-v1:0": true}})
	out, err := GetFoundationModel(context.Background(), "000000000001", "meta.llama3-2-1b-instruct-v1:0")
	require.NoError(t, err)
	require.NotNil(t, out.ModelDetails)
	assert.Equal(t, "meta.llama3-2-1b-instruct-v1:0", *out.ModelDetails.ModelId)
}

func TestGetFoundationModel_SelfHostWithUnresolvableWeightsReturnsNotFound(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	_, err := GetFoundationModel(context.Background(), "000000000001", "meta.llama3-2-1b-instruct-v1:0")
	require.Error(t, err)
	assert.Equal(t, "ResourceNotFoundException", err.Error())
}

func TestGetFoundationModel_UnknownModelReturnsNotFound(t *testing.T) {
	_, err := GetFoundationModel(context.Background(), "000000000001", "does-not-exist")
	require.Error(t, err)
	assert.Equal(t, "ResourceNotFoundException", err.Error())
}

func TestCatalog_SelfHostEntryCarriesServingSpec(t *testing.T) {
	entry, ok := lookupCatalogEntry("meta.llama3-2-1b-instruct-v1:0")
	require.True(t, ok)
	assert.Positive(t, entry.MinVRAMMiB)
	assert.NotEmpty(t, entry.InstanceType)
	assert.NotEmpty(t, entry.VLLMArgs)
}
