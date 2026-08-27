package gateway_bedrock

//test:in-package — exercises the unexported stagedOpenAccessResolver and
// reuses grantSet/grantAll/failingAccess/stubResolver/stubWeightsResolver/
// errWeightsResolver/withWeightsResolver/withProviderCatalogEntry, this
// package's existing unexported test doubles.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// systemBypassInner mimics ModelAccessStore.Granted's GlobalAccountID
// shortcut without a real JetStream-backed store: true only for the system
// account, false for everyone else regardless of model.
type systemBypassInner struct{}

func (systemBypassInner) Granted(_ context.Context, accountID, _ string) (bool, error) {
	return accountID == utils.GlobalAccountID, nil
}

func TestStagedOpenAccessResolver_StagedSelfHostGrantedWithoutInnerGrant(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	r := NewStagedOpenAccessResolver(grantSet{})

	granted, err := r.Granted(context.Background(), "000000000099", selfHostTestModel)
	require.NoError(t, err)
	assert.True(t, granted, "a staged self-host model must be granted to any account")
}

func TestStagedOpenAccessResolver_UnstagedSelfHostNotGranted(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	r := NewStagedOpenAccessResolver(grantSet{})

	granted, err := r.Granted(context.Background(), "000000000099", selfHostTestModel)
	require.NoError(t, err)
	assert.False(t, granted, "an unstaged self-host model must not auto-grant")
}

// TestStagedOpenAccessResolver_ProviderTierStillNeedsInnerGrant covers the
// tier the plan keeps dormant: staging only auto-opens self-host entries.
func TestStagedOpenAccessResolver_ProviderTierStillNeedsInnerGrant(t *testing.T) {
	withProviderCatalogEntry(t, anthropicTestModel)
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	r := NewStagedOpenAccessResolver(grantSet{})

	granted, err := r.Granted(context.Background(), "000000000099", anthropicTestModel)
	require.NoError(t, err)
	assert.False(t, granted, "a provider-tier model must not be auto-granted by staging")
}

func TestStagedOpenAccessResolver_ExplicitInnerGrantStillWins(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	r := NewStagedOpenAccessResolver(grantSet{selfHostTestModel: true})

	granted, err := r.Granted(context.Background(), "000000000001", selfHostTestModel)
	require.NoError(t, err)
	assert.True(t, granted, "an explicit inner grant must still be honored even when unstaged")
}

func TestStagedOpenAccessResolver_SystemAccountBypassIntact(t *testing.T) {
	r := NewStagedOpenAccessResolver(systemBypassInner{})

	granted, err := r.Granted(context.Background(), utils.GlobalAccountID, "nonexistent.model-v1:0")
	require.NoError(t, err)
	assert.True(t, granted, "the inner resolver's system-account bypass must still win")
}

func TestStagedOpenAccessResolver_WeightsResolveErrorPropagates(t *testing.T) {
	withWeightsResolver(t, errWeightsResolver{})
	r := NewStagedOpenAccessResolver(grantSet{})

	_, err := r.Granted(context.Background(), "000000000001", selfHostTestModel)
	require.Error(t, err)
}

func TestStagedOpenAccessResolver_InnerErrorPropagates(t *testing.T) {
	r := NewStagedOpenAccessResolver(failingAccess{})

	_, err := r.Granted(context.Background(), "000000000001", selfHostTestModel)
	require.Error(t, err)
}

func TestStagedOpenAccessResolver_UnknownModelNotGranted(t *testing.T) {
	r := NewStagedOpenAccessResolver(grantSet{})

	granted, err := r.Granted(context.Background(), "000000000001", "nonexistent.model-v1:0")
	require.NoError(t, err)
	assert.False(t, granted)
}

// TestStagedOpenAccessResolver_ListFoundationModelsIncludesStagedSelfHostForNonGrantedAccount
// is the catalog-layer proof that wrapping bedrockAccessResolver's store
// actually reaches ListFoundationModels: a staged self-host model appears for
// an account holding no explicit grant.
func TestStagedOpenAccessResolver_ListFoundationModelsIncludesStagedSelfHostForNonGrantedAccount(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	access := NewStagedOpenAccessResolver(grantSet{})

	out, err := ListFoundationModels(context.Background(), "000000000099", stubResolver{ok: map[string]bool{}}, access, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Contains(t, modelIDs(out), selfHostTestModel)
}

// TestStagedOpenAccessResolver_GetFoundationModelReturnsStagedSelfHostForNonGrantedAccount
// covers the describe path the same way.
func TestStagedOpenAccessResolver_GetFoundationModelReturnsStagedSelfHostForNonGrantedAccount(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	access := NewStagedOpenAccessResolver(grantSet{})

	out, err := GetFoundationModel(context.Background(), "000000000099", selfHostTestModel, access)
	require.NoError(t, err)
	require.NotNil(t, out.ModelDetails)
	assert.Equal(t, selfHostTestModel, *out.ModelDetails.ModelId)
}

// TestStagedOpenAccessResolver_GrantedCatalogEntryAdmitsStagedSelfHostForNonGrantedAccount
// covers the invoke gate every runtime router shares.
func TestStagedOpenAccessResolver_GrantedCatalogEntryAdmitsStagedSelfHostForNonGrantedAccount(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	access := NewStagedOpenAccessResolver(grantSet{})

	entry, err := grantedCatalogEntry(context.Background(), "000000000099", selfHostTestModel, access)
	require.NoError(t, err)
	assert.Equal(t, selfHostTestModel, entry.ModelID)
}
