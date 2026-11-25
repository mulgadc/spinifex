package awsgw_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingEndpoints fails every lookup and counts it, so a case can show a
// refused request never resolved an endpoint.
type countingEndpoints struct {
	mu    sync.Mutex
	calls int
}

var (
	_ ochre.EndpointResolver       = (*countingEndpoints)(nil)
	_ ochre.PinnedEndpointResolver = (*countingEndpoints)(nil)
)

func (c *countingEndpoints) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *countingEndpoints) bump() (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return "", false, nil
}

func (c *countingEndpoints) Endpoint(context.Context, string) (string, bool, error) {
	return c.bump()
}

func (c *countingEndpoints) EndpointForAccount(context.Context, string, string) (string, bool, error) {
	return c.bump()
}

func (c *countingEndpoints) PinnedEndpoint(context.Context, string) (string, bool, error) {
	return c.bump()
}

// capturingRecorder keeps every invocation record the pipeline emits.
type capturingRecorder struct {
	mu      sync.Mutex
	records []ochre.InvocationRecord
}

var _ ochre.Recorder = (*capturingRecorder)(nil)

func (r *capturingRecorder) Record(_ context.Context, rec ochre.InvocationRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *capturingRecorder) mentions(t *testing.T, marker string) bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := json.Marshal(r.records)
	require.NoError(t, err)
	return strings.Contains(string(raw), marker)
}

// denyAllModels grants no model.
type denyAllModels struct{}

func (denyAllModels) Granted(context.Context, string, string) (bool, error) { return false, nil }

// seedLegacySystemCommitment writes an in-service system-account commitment
// on modelID, as one created before such commitments were refused.
func seedLegacySystemCommitment(t *testing.T, env *bedrockPipelineEnv, modelID string) string {
	t.Helper()
	store := kvstore.New[ochre.ProvisionedModelRecord](env.JS, kvstore.Config{Name: "bedrock-provisioned", History: 1})
	now := time.Now().UTC()
	arn := ochre.FormatProvisionedModelARN(gwfix.Region, awsidentifiers.GlobalAccountID, "legacy-embed")
	rec := ochre.ProvisionedModelRecord{
		ARN: arn, ProvisionedModelName: "legacy", ModelID: modelID,
		AccountID: awsidentifiers.GlobalAccountID, ModelUnits: 1,
		Status: bedrock.ProvisionedModelStatusInService, CreationTime: now, LastModifiedTime: now,
	}
	require.NoError(t, store.Set(context.Background(), awsidentifiers.GlobalAccountID+"/legacy-embed", &rec))
	return arn
}

// The input guardrail would intervene on this ConverseStream, so a 400 shows
// the target was refused before the guardrail ran: no endpoint is resolved,
// and the input never reaches an invocation record.
func TestBedrockPipeline_ConverseStreamRefusesTargetBeforeGuardrail(t *testing.T) {
	t.Parallel()
	const embedModel = "nomic-embed-text-v1.5"
	const marker = "blockedmarker"
	cases := []struct {
		name   string
		caller string
		access ochre.AccessResolver
		target func(t *testing.T, env *bedrockPipelineEnv) string
		status int
		errTyp string
	}{
		{
			name: "embedding model", caller: authzAccountID, access: grantAllModels{},
			target: func(*testing.T, *bedrockPipelineEnv) string { return embedModel },
			status: http.StatusBadRequest, errTyp: "ValidationException",
		},
		{
			name: "legacy system commitment on the embedding bundle", caller: awsidentifiers.GlobalAccountID, access: grantAllModels{},
			target: func(t *testing.T, env *bedrockPipelineEnv) string {
				return seedLegacySystemCommitment(t, env, bedrockPipelineLlamaModel)
			},
			status: http.StatusBadRequest, errTyp: "ValidationException",
		},
		{
			name: "denied embedding model", caller: authzAccountID, access: denyAllModels{},
			target: func(*testing.T, *bedrockPipelineEnv) string { return embedModel },
			status: http.StatusForbidden, errTyp: "AccessDeniedException",
		},
		{
			// On a bundle member with no staged weights, so nothing grants it.
			name: "denied legacy system commitment", caller: awsidentifiers.GlobalAccountID, access: denyAllModels{},
			target: func(t *testing.T, env *bedrockPipelineEnv) string {
				return seedLegacySystemCommitment(t, env, embedModel)
			},
			status: http.StatusForbidden, errTyp: "AccessDeniedException",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoints := &countingEndpoints{}
			recorder := &capturingRecorder{}
			var target string
			app := bedrockPipelineApp(t, func(env *bedrockPipelineEnv) {
				env.CallerAccountID = tc.caller
				env.Deps.Endpoints = endpoints
				env.Deps.Recorder = recorder
				env.Deps.Access = tc.access
				target = tc.target(t, env)
			}, allowBedrock())

			create := probeBedrock(t, app, signBedrockRequest(t, "bedrock", http.MethodPost, "/guardrails",
				[]byte(`{"name":"g","blockedInputMessaging":"x","blockedOutputsMessaging":"y","wordPolicyConfig":{"wordsConfig":[{"text":"`+marker+`"}]}}`)))
			require.Equal(t, http.StatusOK, create.Status, create.Body)
			var created struct {
				GuardrailID string `json:"guardrailId"`
			}
			require.NoError(t, json.Unmarshal([]byte(create.Body), &created))
			require.NotEmpty(t, created.GuardrailID)

			body := []byte(`{"messages":[{"role":"user","content":[{"text":"` + marker + `"}]}],` +
				`"guardrailConfig":{"guardrailIdentifier":"` + created.GuardrailID + `","guardrailVersion":"DRAFT"}}`)
			path := "/model/" + url.PathEscape(target) + "/converse-stream"
			got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", http.MethodPost, path, body))

			assert.Equal(t, tc.status, got.Status, got.Body)
			assert.Equal(t, tc.errTyp, got.Type)
			assert.Zero(t, endpoints.count(), "a refused target resolves no endpoint")
			assert.False(t, recorder.mentions(t, marker), "a refused request records no input")
		})
	}
}
