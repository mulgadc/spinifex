package gateway_bedrock

//test:in-package — shares unexported test doubles (grantSet, grantAll,
// stubResolver, withWeightsResolver, withProviderCatalogEntry) with the rest
// of this package's *_test.go files rather than duplicating them behind an
// exported surface.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func adminEntry(t *testing.T, entries []AdminCatalogEntry, modelID string) AdminCatalogEntry {
	t.Helper()
	for _, e := range entries {
		if e.ModelID == modelID {
			return e
		}
	}
	require.Failf(t, "model not found in admin catalog", "modelID=%q", modelID)
	return AdminCatalogEntry{}
}

// TestAdminCatalog_ClaudeEntryAbsent covers the v1 removal directly: the
// shipped catalog carries no Anthropic entry at all, so it cannot appear in
// the admin read even for the operator account.
func TestAdminCatalog_ClaudeEntryAbsent(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	entries, err := AdminCatalog(context.Background(), "000000000000", stubResolver{ok: map[string]bool{}}, grantAll{})
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, anthropicTestModel, e.ModelID)
	}
}

// TestAdminCatalog_ReasonUngranted covers the ungranted case for the
// provider tier, the only one still gated by an explicit grant: self-host is
// judged weights-first and never reports ungranted (see
// TestAdminCatalog_SelfHostNeverUngranted).
func TestAdminCatalog_ReasonUngranted(t *testing.T) {
	withProviderCatalogEntry(t, anthropicTestModel)
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}}, grantSet{})
	require.NoError(t, err)
	assert.Equal(t, AvailabilityUngranted, adminEntry(t, entries, anthropicTestModel).Availability)
}

// TestAdminCatalog_SelfHostAvailableWithoutGrant is import-is-the-grant at
// the admin layer: a staged self-host model reports available even for an
// account holding no explicit grant on it.
func TestAdminCatalog_SelfHostAvailableWithoutGrant(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, grantSet{})
	require.NoError(t, err)
	assert.Equal(t, AvailabilityAvailable, adminEntry(t, entries, selfHostTestModel).Availability)
}

// TestAdminCatalog_SelfHostNeverUngranted proves no self-host catalog entry
// can report AvailabilityUngranted, staged or not: the reason reserved for
// the provider tier must never leak onto a self-host entry.
func TestAdminCatalog_SelfHostNeverUngranted(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, grantSet{})
	require.NoError(t, err)
	for _, e := range entries {
		entry, ok := lookupCatalogEntry(e.ModelID)
		if !ok || entry.Provider != tierSelfHost {
			continue
		}
		assert.NotEqual(t, AvailabilityUngranted, e.Availability, "self-host model %q must never report ungranted", e.ModelID)
	}
}

// TestAdminCatalog_ReasonNoWeightsStaged covers a granted self-host model
// whose weights snapshot does not resolve.
func TestAdminCatalog_ReasonNoWeightsStaged(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, grantAll{})
	require.NoError(t, err)
	assert.Equal(t, AvailabilityNoWeights, adminEntry(t, entries, selfHostTestModel).Availability)
}

// TestAdminCatalog_ReasonNoCredential covers a granted provider model whose
// vendor credential does not resolve. v1 ships no provider entry, so this
// synthesizes one the same way the router tests do.
func TestAdminCatalog_ReasonNoCredential(t *testing.T) {
	withProviderCatalogEntry(t, anthropicTestModel)
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, grantAll{})
	require.NoError(t, err)
	assert.Equal(t, AvailabilityNoCredential, adminEntry(t, entries, anthropicTestModel).Availability)
}

// TestAdminCatalog_ReasonAvailable covers the fully-servable case for both
// tiers: granted, and weights/credential resolve.
func TestAdminCatalog_ReasonAvailable(t *testing.T) {
	withProviderCatalogEntry(t, anthropicTestModel)
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}}, grantAll{})
	require.NoError(t, err)
	assert.Equal(t, AvailabilityAvailable, adminEntry(t, entries, selfHostTestModel).Availability)
	assert.Equal(t, AvailabilityAvailable, adminEntry(t, entries, anthropicTestModel).Availability)
}

// TestAdminCatalog_CarriesOpsMetadata covers the fields AWS's own wire shape
// cannot carry: VRAM floor, instance type and co-serve group must survive
// the admin projection unchanged from the catalog entry.
func TestAdminCatalog_CarriesOpsMetadata(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true}})
	entries, err := AdminCatalog(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}}, grantAll{})
	require.NoError(t, err)
	entry := adminEntry(t, entries, selfHostTestModel)
	assert.Positive(t, entry.MinVRAMMiB)
	assert.NotEmpty(t, entry.InstanceType)
	assert.Equal(t, coServeGroupOchreDemo, entry.CoServeGroup)
}

// TestListFoundationModels_NeverLeaksAvailabilityReason is the tenant-safety
// guard: the wire JSON for the public catalog read must never carry an
// availability/reason field, regardless of why a model was omitted, since a
// tenant credential must not learn why (or whether) a model exists.
func TestListFoundationModels_NeverLeaksAvailabilityReason(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{}},
		grantSet{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.Empty(t, modelIDs(out), "an ungranted, unresolvable account must see an empty catalog")

	body, err := json.Marshal(out)
	require.NoError(t, err)
	lower := strings.ToLower(string(body))
	assert.NotContains(t, lower, "availab")
	assert.NotContains(t, lower, "reason")
	assert.NotContains(t, lower, "ungranted")
	assert.NotContains(t, lower, "credential")
}

// TestListFoundationModels_ClaudeEntryAbsent pins the tenant-facing catalog
// to the same v1 removal AdminCatalog observes.
func TestListFoundationModels_ClaudeEntryAbsent(t *testing.T) {
	withWeightsResolver(t, stubWeightsResolver{ok: map[string]bool{selfHostTestModel: true, selfHostTestModel3B: true}})
	out, err := ListFoundationModels(context.Background(), "000000000001", stubResolver{ok: map[string]bool{"anthropic": true}},
		grantAll{}, &bedrock.ListFoundationModelsInput{})
	require.NoError(t, err)
	assert.NotContains(t, modelIDs(out), anthropicTestModel)
}
