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

func TestListFoundationModels_SelfHostIncludedWhenGranted(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantSet{selfHostTestModel: true}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), selfHostTestModel)
}

// TestListFoundationModels_SelfHostExcludedWhenUngranted is the behaviour
// change: self-host entries used to be advertised to every account
// unconditionally, so this is the regression guard for that.
func TestListFoundationModels_SelfHostExcludedWhenUngranted(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantSet{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), selfHostTestModel)
	assert.Empty(t, modelIDs(out), "an account with no grants must see an empty catalog")
}

func TestListFoundationModels_ProviderIncludedWhenGrantedAndResolvable(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}},
		grantSet{anthropicTestModel: true}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), anthropicTestModel)
}

func TestListFoundationModels_ProviderExcludedWhenUnresolvable(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantSet{anthropicTestModel: true}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), anthropicTestModel)
}

// TestListFoundationModels_GrantDoesNotOverrideCredentialTier keeps the two
// filters independent: a grant says the account may use the model, not that
// the platform can reach it.
func TestListFoundationModels_ProviderExcludedWhenUngrantedButResolvable(t *testing.T) {
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}},
		grantSet{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), anthropicTestModel,
		"a platform-default credential must not advertise a model the account was never granted")
}

// TestCatalogModelIDs_CoversWholeCatalog keeps the admin-facing listing in step
// with the catalog: a model missing here would be ungrantable via --all-models
// and so invisible to every account.
func TestCatalogModelIDs_CoversWholeCatalog(t *testing.T) {
	ids := CatalogModelIDs()
	require.Len(t, ids, len(catalog))
	assert.Contains(t, ids, selfHostTestModel)
	assert.Contains(t, ids, anthropicTestModel)
}

func TestGetFoundationModel_KnownGrantedModel(t *testing.T) {
	out, err := GetFoundationModel(context.Background(), "000000000001", selfHostTestModel, grantSet{selfHostTestModel: true})
	require.NoError(t, err)
	require.NotNil(t, out.ModelDetails)
	assert.Equal(t, selfHostTestModel, *out.ModelDetails.ModelId)
}

func TestGetFoundationModel_UnknownModelReturnsNotFound(t *testing.T) {
	_, err := GetFoundationModel(context.Background(), "000000000001", "does-not-exist", grantAll{})
	require.Error(t, err)
	assert.Equal(t, "ResourceNotFoundException", err.Error())
}

// TestGetFoundationModel_UngrantedModelReturnsNotFound pins describe to the
// same answer as list: an ungranted model is reported as absent rather than
// forbidden, so the error cannot be used to confirm the model exists.
func TestGetFoundationModel_UngrantedModelReturnsNotFound(t *testing.T) {
	_, err := GetFoundationModel(context.Background(), "000000000001", selfHostTestModel, grantSet{})
	require.Error(t, err)
	assert.Equal(t, "ResourceNotFoundException", err.Error())
}
