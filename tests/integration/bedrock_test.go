//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelSelfHostLlama and modelAnthropic mirror the static Phase-1 catalog in
// gateway/bedrock/catalog.go — see tests/e2e/bedrock/bedrock_test.go, whose
// TestListFoundationModels this ports.
const (
	modelSelfHostLlama = "meta.llama3-2-1b-instruct-v1:0"
	modelAnthropic     = "anthropic.claude-3-5-sonnet-20240620-v1:0"
)

// integrationWeightsStub resolves a weights snapshot only for
// modelSelfHostLlama, standing in for the KV override that gates a self-host
// catalog entry on having somewhere to serve it from.
type integrationWeightsStub struct{}

func (integrationWeightsStub) Resolve(_ context.Context, modelID string) (string, bool, error) {
	if modelID != modelSelfHostLlama {
		return "", false, nil
	}
	return "snap-stub", true, nil
}

// TestListFoundationModels ports tests/e2e/bedrock/bedrock_test.go's
// TestListFoundationModels. ListFoundationModels is pure gateway-side static
// catalog logic (gateway/bedrock/catalog.go) with no NATS/daemon hop at all —
// no guest, no daemon wiring needed — so it round-trips through the real
// gateway HTTP/SigV4 path exactly like the live test, without a live cluster.
func TestListFoundationModels(t *testing.T) {
	// Not t.Parallel(): ListFoundationModels/GetFoundationModel read the
	// weights resolver through a package-level setter (StartGateway builds a
	// GatewayConfig directly, with no field for it), so this test owns that
	// process-wide state for its duration.
	gateway_bedrock.SetWeightsResolver(integrationWeightsStub{})
	t.Cleanup(func() { gateway_bedrock.SetWeightsResolver(nil) })

	gw := StartGateway(t)
	bedrockCli := gw.BedrockClient(t)

	out, err := bedrockCli.ListFoundationModels(&bedrock.ListFoundationModelsInput{})
	require.NoError(t, err, "list-foundation-models")

	ids := make(map[string]*bedrock.FoundationModelSummary, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		ids[aws.StringValue(m.ModelId)] = m
	}

	assert.Contains(t, ids, modelSelfHostLlama, "self-host model must be advertised when its weights resolve")

	// This tier configures no BedrockCredentials resolver, so the Anthropic
	// entry is never resolvable and must not be advertised — the mirror image
	// of the live test's "only assert when present" check (which tolerates
	// either state depending on live cluster credential configuration).
	_, anthropicPresent := ids[modelAnthropic]
	assert.False(t, anthropicPresent, "anthropic model advertised without a resolvable credential")
}
