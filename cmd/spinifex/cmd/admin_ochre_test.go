package cmd

import (
	"testing"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ochreFlagCmd builds a bare command carrying the model-selection flags, so
// the resolver can be exercised without the cluster connection the real
// subcommands make.
func ochreFlagCmd(t *testing.T, modelID string, allModels bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("model-id", modelID, "")
	cmd.Flags().Bool("all-models", allModels, "")
	return cmd
}

func TestOchreTargetModels_SingleModel(t *testing.T) {
	models, err := ochreTargetModels(ochreFlagCmd(t, "meta.llama3-70b-instruct-v1:0", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"meta.llama3-70b-instruct-v1:0"}, models)
}

func TestOchreTargetModels_AllModels(t *testing.T) {
	models, err := ochreTargetModels(ochreFlagCmd(t, "", true))
	require.NoError(t, err)
	assert.Equal(t, gateway_bedrock.CatalogModelIDs(), models)
	assert.NotEmpty(t, models)
}

// TestOchreTargetModels_NeitherFlag pins the refusal to guess: silently
// defaulting to the whole catalog would over-grant on a typo.
func TestOchreTargetModels_NeitherFlag(t *testing.T) {
	_, err := ochreTargetModels(ochreFlagCmd(t, "", false))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--model-id or --all-models")
}

func TestOchreTargetModels_BothFlags(t *testing.T) {
	_, err := ochreTargetModels(ochreFlagCmd(t, "meta.llama3-70b-instruct-v1:0", true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}
