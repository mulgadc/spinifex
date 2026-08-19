package gateway_bedrock

import (
	"context"
	"errors"
	"strings"
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

// errWeightsResolver simulates a broken resolver (e.g. a JetStream outage):
// every Resolve call fails rather than cleanly reporting not-found.
type errWeightsResolver struct{}

func (errWeightsResolver) Resolve(_ context.Context, _ string) (string, bool, error) {
	return "", false, errors.New("kv unavailable")
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
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantAll{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), selfHostTestModel)
}

func TestListFoundationModels_SelfHostExcludedWhenWeightsUnresolvable(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantAll{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), selfHostTestModel)
}

func TestListFoundationModels_SelfHostIncludedWhenGranted(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantSet{selfHostTestModel: true}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), selfHostTestModel)
}

// TestListFoundationModels_SelfHostExcludedWhenUngranted is the behaviour
// change: self-host entries used to be advertised to every account
// unconditionally, so this is the regression guard for that. The weights
// resolve here, so the exclusion can only be the missing grant.
func TestListFoundationModels_SelfHostExcludedWhenUngranted(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
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
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantSet{anthropicTestModel: true}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), anthropicTestModel)
}

func TestGetFoundationModel_KnownModelWithResolvableWeights(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	out, err := GetFoundationModel(context.Background(), "000000000001", selfHostTestModel, grantAll{})
	require.NoError(t, err)
	require.NotNil(t, out.ModelDetails)
	assert.Equal(t, selfHostTestModel, *out.ModelDetails.ModelId)
}

func TestGetFoundationModel_SelfHostWithUnresolvableWeightsReturnsNotFound(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	_, err := GetFoundationModel(context.Background(), "000000000001", selfHostTestModel, grantAll{})
	require.Error(t, err)
	assert.Equal(t, "ResourceNotFoundException", err.Error())
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
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
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

func TestGetFoundationModel_WeightsResolveErrorIsNotResourceNotFound(t *testing.T) {
	withWeightsResolver(t, errWeightsResolver{})
	_, err := GetFoundationModel(context.Background(), "000000000001", selfHostTestModel, grantAll{})
	require.Error(t, err)
	assert.NotEqual(t, "ResourceNotFoundException", err.Error())
}

func TestListFoundationModels_WeightsResolveErrorPropagates(t *testing.T) {
	withWeightsResolver(t, errWeightsResolver{})
	_, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantAll{}, &bedrock.ListFoundationModelsInput{})
	require.Error(t, err)
	assert.NotEqual(t, "ResourceNotFoundException", err.Error())
}

func TestCatalog_SelfHostEntryCarriesServingSpec(t *testing.T) {
	entry, ok := lookupCatalogEntry("meta.llama3-2-1b-instruct-v1:0")
	require.True(t, ok)
	assert.Positive(t, entry.MinVRAMMiB)
	assert.NotEmpty(t, entry.InstanceType)
	assert.NotEmpty(t, entry.VLLMArgs)
}

// TestLookupServingSpec_SelfHostEntry covers 'ochre weights stage's happy
// path: a known self-host model resolves found=true, selfHost=true, and a
// non-empty serving spec.
func TestLookupServingSpec_SelfHostEntry(t *testing.T) {
	spec, found, selfHost := LookupServingSpec("meta.llama3-2-1b-instruct-v1:0")
	require.True(t, found)
	require.True(t, selfHost)
	assert.Equal(t, "meta.llama3-2-1b-instruct-v1:0", spec.ModelID)
	assert.NotEmpty(t, spec.InstanceType)
	assert.Positive(t, spec.MinVRAMMiB)
	assert.NotEmpty(t, spec.VLLMArgs)
}

// TestLookupServingSpec_ProviderEntry covers the refusal case 'stage' must
// hit for a valid-but-wrong-tier model ID: found=true (it IS a catalog
// entry), selfHost=false, so staging must be refused rather than proceed.
func TestLookupServingSpec_ProviderEntry(t *testing.T) {
	spec, found, selfHost := LookupServingSpec("anthropic.claude-3-5-sonnet-20240620-v1:0")
	require.True(t, found)
	assert.False(t, selfHost)
	assert.Zero(t, spec)
}

// TestLookupServingSpec_UnknownModel covers the other refusal case: a
// typo'd or nonexistent model ID must report found=false, not merely
// selfHost=false, so the CLI can give a precise error either way.
func TestLookupServingSpec_UnknownModel(t *testing.T) {
	spec, found, selfHost := LookupServingSpec("not-a-real-model")
	assert.False(t, found)
	assert.False(t, selfHost)
	assert.Zero(t, spec)
}

func TestLookupServingSpec_SelfHost3BEntry(t *testing.T) {
	spec, found, selfHost := LookupServingSpec(selfHostTestModel3B)
	require.True(t, found)
	require.True(t, selfHost)
	assert.Equal(t, selfHostTestModel3B, spec.ModelID)
	assert.Equal(t, "g5.xlarge", spec.InstanceType)
	assert.Equal(t, 7168, spec.MinVRAMMiB)
	assert.Contains(t, spec.VLLMArgs, "--enforce-eager",
		"CUDA graph capture does not fit alongside 3B of bf16 weights on an 8 GiB card")
}

// TestCatalog_SelfHostEntriesFitTheLlamaInvokeAdapter guards the assumption
// baked into selfHostInvokeAdapter: every familyMeta entry is Llama-shaped, so
// it is safe to dispatch through llamaInvokeAdapter. A non-meta family (TEI's
// embedder/reranker included) is unhandled and refused instead, not silently
// served with the wrong native schema.
func TestCatalog_SelfHostEntriesFitTheLlamaInvokeAdapter(t *testing.T) {
	for _, entry := range catalog {
		if entry.Provider != tierSelfHost || entry.Family != familyMeta {
			continue
		}
		assert.True(t, strings.HasPrefix(entry.ModelID, "meta.llama"),
			"familyMeta entry %q is not a Llama model; InvokeRouter would serve it "+
				"through llamaInvokeAdapter with the wrong native schema", entry.ModelID)
	}
}

// TestCoServeGroupVRAMMiB_StandaloneModel covers the bundle-of-one case: a
// self-host entry with no CoServeGroup resolves to just its own floor.
func TestCoServeGroupVRAMMiB_StandaloneModel(t *testing.T) {
	total, members, found := CoServeGroupVRAMMiB(selfHostTestModel3B)
	require.True(t, found)
	assert.Equal(t, 7168, total)
	assert.Equal(t, []string{selfHostTestModel3B}, members)
}

// TestCoServeGroupVRAMMiB_BundleSumsMembers covers the populated demo bundle:
// the LLM, embedder and reranker share one group, so admission must see one
// summed floor rather than three independent whole-GPU claims.
func TestCoServeGroupVRAMMiB_BundleSumsMembers(t *testing.T) {
	total, members, found := CoServeGroupVRAMMiB(selfHostTestModel)
	require.True(t, found)
	assert.Equal(t, 5120+512+1200, total)
	assert.ElementsMatch(t, []string{selfHostTestModel, "nomic-embed-text-v1.5", "bge-reranker-v2-m3"}, members)
}

// TestCoServeGroupVRAMMiB_UnknownModel reports found=false rather than a
// zero-value bundle, so callers can distinguish "no such model" from a
// legitimately tiny group.
func TestCoServeGroupVRAMMiB_UnknownModel(t *testing.T) {
	total, members, found := CoServeGroupVRAMMiB("not-a-real-model")
	assert.False(t, found)
	assert.Zero(t, total)
	assert.Nil(t, members)
}

// TestLookupCoServeGroup_StandaloneModel covers the bundle-of-one case: a
// self-host entry with no CoServeGroup resolves to a group of one, GroupID
// equal to its own ModelID, VLLMArgs unchanged from the catalog entry —
// today's single-model behaviour, not a special case of it.
func TestLookupCoServeGroup_StandaloneModel(t *testing.T) {
	spec, found, selfHost := LookupCoServeGroup(selfHostTestModel3B)
	require.True(t, found)
	require.True(t, selfHost)
	assert.Equal(t, selfHostTestModel3B, spec.GroupID)
	assert.Equal(t, 7168, spec.TotalMinVRAMMiB)
	assert.Equal(t, "g5.xlarge", spec.InstanceType)
	require.Len(t, spec.Members, 1)
	assert.Equal(t, selfHostTestModel3B, spec.Members[0].ModelID)
	entry, _ := lookupCatalogEntry(selfHostTestModel3B)
	assert.Equal(t, entry.VLLMArgs, spec.Members[0].VLLMArgs,
		"a standalone model's VLLMArgs must be untouched by bundle scaling")
}

// TestLookupCoServeGroup_Bundle covers the populated demo bundle: every
// member is present, the VRAM floor matches CoServeGroupVRAMMiB's own sum,
// and the vLLM member's --gpu-memory-utilization is scaled down from its
// solo value to leave headroom for the TEI members sharing the card, while
// the TEI members keep no vLLM-specific args at all.
func TestLookupCoServeGroup_Bundle(t *testing.T) {
	spec, found, selfHost := LookupCoServeGroup(selfHostTestModel)
	require.True(t, found)
	require.True(t, selfHost)
	assert.Equal(t, "ochre-demo-bundle", spec.GroupID)
	assert.Equal(t, 5120+512+1200, spec.TotalMinVRAMMiB)
	require.Len(t, spec.Members, 3)

	byModel := make(map[string]CoServeMember, len(spec.Members))
	for _, m := range spec.Members {
		byModel[m.ModelID] = m
	}
	llm, ok := byModel[selfHostTestModel]
	require.True(t, ok)
	assert.Equal(t, FamilyMeta, llm.Family)
	require.NotEmpty(t, llm.VLLMArgs)
	var utilArg string
	for _, a := range llm.VLLMArgs {
		if strings.HasPrefix(a, "--gpu-memory-utilization=") {
			utilArg = a
		}
	}
	require.NotEmpty(t, utilArg, "the vLLM member must still carry a utilization cap")
	assert.NotEqual(t, "--gpu-memory-utilization=0.6", utilArg,
		"the bundle must not reuse the standalone solo-launch utilization unchanged")

	embed, ok := byModel["nomic-embed-text-v1.5"]
	require.True(t, ok)
	assert.Equal(t, FamilyTEI, embed.Family)

	rerank, ok := byModel["bge-reranker-v2-m3"]
	require.True(t, ok)
	assert.Equal(t, FamilyTEI, rerank.Family)
}

// TestLookupCoServeGroup_ProviderEntry covers the refusal case a launcher
// must hit for a valid-but-wrong-tier model ID.
func TestLookupCoServeGroup_ProviderEntry(t *testing.T) {
	spec, found, selfHost := LookupCoServeGroup(anthropicTestModel)
	require.True(t, found)
	assert.False(t, selfHost)
	assert.Zero(t, spec)
}

// TestLookupCoServeGroup_UnknownModel reports found=false rather than a
// zero-value group, so callers can distinguish "no such model" from a
// legitimately tiny group.
func TestLookupCoServeGroup_UnknownModel(t *testing.T) {
	spec, found, selfHost := LookupCoServeGroup("not-a-real-model")
	assert.False(t, found)
	assert.False(t, selfHost)
	assert.Zero(t, spec)
}

// TestListFoundationModels_SelfHostGatingIsPerEntry proves weights gating
// filters one entry at a time rather than all-or-nothing. Only measurable now
// that two self-host entries exist and can disagree.
func TestListFoundationModels_SelfHostGatingIsPerEntry(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel3B: true}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantAll{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), selfHostTestModel3B)
	assert.NotContains(t, modelIDs(out), selfHostTestModel,
		"a staged model must not advertise an unstaged sibling")
}
