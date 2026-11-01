// Characterization tests for the four Bedrock services through the full SigV4
// pipeline as bedrock*.go implement them today. They assert current
// behaviour, not AWS parity.

package awsgw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	ochreawsapi "github.com/mulgadc/spinifex/spinifex/domains/ochre/awsapi"
	ochrevector "github.com/mulgadc/spinifex/spinifex/domains/ochre/vector"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bedrockPipelineModelPath = "/model/" + bedrockPipelineLlamaModel

var retrieveBody = []byte(`{"retrievalQuery":{"text":"q"}}`)

const bedrockPipelineLlamaModel = "meta.llama3-2-1b-instruct-v1:0"

// grantAllModels grants every model, standing in for a provisioned grant store.
type grantAllModels struct{}

func (grantAllModels) Granted(_ context.Context, _, _ string) (bool, error) { return true, nil }

// weightsStub resolves a weights snapshot for the one self-host model the
// pipeline tests name.
type weightsStub struct{}

func (weightsStub) Resolve(_ context.Context, modelID string) (string, bool, error) {
	if modelID != bedrockPipelineLlamaModel {
		return "", false, nil
	}
	return "snap-stub", true, nil
}

// bedrockPipelineEnv is what a case may change before the app is built.
type bedrockPipelineEnv struct {
	Deps   ochreawsapi.Deps
	NoNATS bool
}

// bedrockPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does,
// with the Bedrock registration over a real JetStream and every capability set
// unless mutate clears it. Policy statements are the caller's only IAM policy.
func bedrockPipelineApp(t *testing.T, mutate func(*bedrockPipelineEnv), statements ...iamdomain.Statement) http.Handler {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	svc := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       authzAccountID,
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
	}
	_, nc, js := testutil.StartTestJetStream(t)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	}))
	t.Cleanup(stub.Close)
	ochre.SetWeightsResolver(weightsStub{})
	t.Cleanup(func() { ochre.SetWeightsResolver(nil) })

	env := &bedrockPipelineEnv{Deps: ochreawsapi.Deps{
		Endpoints:   ochre.NewStaticEndpointResolver(map[string]string{bedrockPipelineLlamaModel: stub.URL}),
		Access:      grantAllModels{},
		Logging:     ochre.NewLoggingConfigStore(js),
		Provisioned: ochre.NewProvisionedStore(js, gwfix.Region, nil),
		Guardrails:  ochre.NewGuardrailStore(js, gwfix.Region),
		KB:          ochrevector.NewKBStore(js),
		DataSources: ochrevector.NewDataSourceStore(js),
		Vector:      unusedVector{},
	}}
	if mutate != nil {
		mutate(env)
	}
	cfg := gwfix.WithDeps(&gateway.GatewayConfig{DisableLogging: true, Region: gwfix.Region}, gwfix.Deps{NATS: nc, IAM: svc})
	if env.NoNATS {
		gwfix.DepsOf(cfg).NATS = nil
	}
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(ochreawsapi.NewRegistration(env.Deps)))
	cfg.Services = b.Build()
	gw := withVerifier(cfg)
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return r
}

// unusedVector fails the test run if a request reaches the vector engine.
type unusedVector struct{ ochrevector.VectorService }

func signBedrockRequest(t *testing.T, scope, method, path string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, scope, time.Now().UTC())
	return req
}

type bedrockOutcome struct {
	Status      int
	Type        string
	Header      string
	ContentType string
	Body        string
}

func probeBedrock(t *testing.T, app http.Handler, req *http.Request) bedrockOutcome {
	t.Helper()
	resp := doRequest(app, req)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := bedrockOutcome{
		Status:      resp.StatusCode,
		Header:      resp.Header.Get("X-Amzn-Errortype"),
		ContentType: resp.Header.Get("Content-Type"),
		Body:        string(raw),
	}
	if resp.StatusCode >= 400 && strings.HasPrefix(out.Body, "{") {
		var env jsonErrorEnvelope
		require.NoError(t, json.Unmarshal(raw, &env), string(raw))
		out.Type = env.Type
	}
	return out
}

func allowBedrock() iamdomain.Statement { return statement("Allow", "bedrock:*", "*") }

// TestBedrockPipeline_ErrorEnvelopes pins status, error type, the
// X-Amzn-Errortype header and the JSON 1.1 content type for each service.
func TestBedrockPipeline_ErrorEnvelopes(t *testing.T) {
	const jsonCT = "application/x-amz-json-1.1"
	guardrail := []byte(`{"name":"g","blockedInputMessaging":"x","blockedOutputsMessaging":"y"}`)

	cases := []struct {
		name   string
		app    http.Handler
		req    *http.Request
		status int
		typ    string
	}{
		{"bedrock unknown route", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "GET", "/nope", nil), 400, "InvalidActionException"},
		{"bedrock policy deny", bedrockPipelineApp(t, nil), signBedrockRequest(t, "bedrock", "GET", "/guardrails", nil), 403, "AccessDeniedException"},
		{"bedrock malformed body", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", "/guardrails", []byte("{")), 400, "ValidationException"},
		{"bedrock not found", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "GET", "/guardrails/gr-none", nil), 404, "ResourceNotFoundException"},
		{"bedrock ok", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", "/guardrails", guardrail), 200, ""},
		{"bedrock no nats", bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.NoNATS = true }, allowBedrock()), signBedrockRequest(t, "bedrock", "GET", "/guardrails", nil), 503, "ServiceUnavailableException"},
		{"runtime unknown route", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", "/model/m/nope", nil), 400, "InvalidActionException"},
		{"runtime policy deny", bedrockPipelineApp(t, nil), signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse", []byte("{}")), 403, "AccessDeniedException"},
		{"runtime malformed body", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse", []byte("{")), 400, "ValidationException"},
		{"runtime no nats", bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.NoNATS = true }, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse", []byte("{}")), 503, "ServiceUnavailableException"},
		{"runtime stream pre-frame error", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse-stream", []byte("{")), 400, "ValidationException"},
		{"agent unknown route", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "DELETE", "/knowledgebases/", nil), 400, "InvalidActionException"},
		{"agent policy deny", bedrockPipelineApp(t, nil), signBedrockRequest(t, "bedrock", "GET", "/knowledgebases/kb1", nil), 403, "AccessDeniedException"},
		{"agent not found", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "GET", "/knowledgebases/kb1", nil), 404, "ResourceNotFoundException"},
		{"agent no vector", bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Vector = nil }, allowBedrock()), signBedrockRequest(t, "bedrock", "GET", "/knowledgebases/kb1", nil), 500, "ServerInternalException"},
		{"agent-runtime unknown route", bedrockPipelineApp(t, nil, allowBedrock()), signBedrockRequest(t, "bedrock", "GET", "/retrieveAndGenerate", nil), 400, "InvalidActionException"},
		{"agent-runtime policy deny", bedrockPipelineApp(t, nil), signBedrockRequest(t, "bedrock", "POST", "/knowledgebases/kb1/retrieve", retrieveBody), 403, "AccessDeniedException"},
		{"agent-runtime no vector", bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Vector = nil }, allowBedrock()), signBedrockRequest(t, "bedrock", "POST", "/knowledgebases/kb1/retrieve", retrieveBody), 500, "ServerInternalException"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := probeBedrock(t, c.app, c.req)
			require.Equal(t, c.status, got.Status, got.Body)
			require.Equal(t, c.typ, got.Type, got.Body)
			if c.status >= 400 {
				require.Equal(t, c.typ, got.Header)
				require.Equal(t, jsonCT, got.ContentType)
			}
		})
	}
}

// TestBedrockPipeline_ScopeNames pins which credential-scope names reach the
// Bedrock dispatchers. Real SDKs sign "bedrock"; the three sub-service names
// are accepted too and the request path alone picks the dispatcher.
func TestBedrockPipeline_ScopeNames(t *testing.T) {
	cases := []struct {
		name   string
		scope  string
		method string
		path   string
		status int
		typ    string
	}{
		{"bedrock scope on bedrock path", "bedrock", "GET", "/guardrails", 200, ""},
		{"bedrock-runtime scope on bedrock path", "bedrock-runtime", "GET", "/guardrails", 400, "InvalidActionException"},
		{"bedrock-agent scope on bedrock path", "bedrock-agent", "GET", "/guardrails", 400, "InvalidActionException"},
		{"bedrock-agent-runtime scope on bedrock path", "bedrock-agent-runtime", "GET", "/guardrails", 400, "InvalidActionException"},
		{"bedrock-agent scope on agent path", "bedrock-agent", "GET", "/knowledgebases/kb1", 404, "ResourceNotFoundException"},
		{"bedrock-runtime scope on runtime path", "bedrock-runtime", "POST", bedrockPipelineModelPath + "/converse", 400, "ValidationException"},
		{"bedrock-agent-runtime scope on retrieve path", "bedrock-agent-runtime", "POST", "/knowledgebases/kb1/retrieve", 404, "ResourceNotFoundException"},
		{"unknown bedrock scope", "bedrock-foo", "GET", "/guardrails", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(nil)
			if c.method == "POST" {
				body = []byte("{")
				if strings.HasSuffix(c.path, "/retrieve") {
					body = retrieveBody
				}
			}
			app := bedrockPipelineApp(t, nil, allowBedrock())
			got := probeBedrock(t, app, signBedrockRequest(t, c.scope, c.method, c.path, body))
			if c.status == 0 {
				assert.GreaterOrEqual(t, got.Status, 400, got.Body)
				assert.True(t, strings.HasPrefix(got.Body, "<?xml"), "unknown scope answers in XML: %s", got.Body)
				return
			}
			require.Equal(t, c.status, got.Status, got.Body)
			require.Equal(t, c.typ, got.Type, got.Body)
		})
	}
}

// TestBedrockPipeline_PathSplit pins which dispatcher a path reaches by the
// error each one gives to the same empty-store request.
func TestBedrockPipeline_PathSplit(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		typ    string
	}{
		{"guardrail apply is runtime", "POST", "/guardrail/g1/version/1/apply", "{", 400, "ValidationException"},
		{"guardrails is bedrock", "GET", "/guardrails/g1", "", 404, "ResourceNotFoundException"},
		{"retrieveAndGenerate is agent-runtime", "POST", "/retrieveAndGenerate", "{", 400, ""},
		{"kb retrieve is agent-runtime", "POST", "/knowledgebases/kb1/retrieve", `{"retrievalQuery":{"text":"q"}}`, 404, "ResourceNotFoundException"},
		{"kb get is agent", "GET", "/knowledgebases/kb1", "", 404, "ResourceNotFoundException"},
		{"kb trailing retrieve on get is unknown to agent-runtime", "GET", "/knowledgebases/kb1/retrieve", "", 400, "InvalidActionException"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := bedrockPipelineApp(t, nil, allowBedrock())
			var body []byte
			if c.body != "" {
				body = []byte(c.body)
			}
			got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", c.method, c.path, body))
			require.Equal(t, c.status, got.Status, got.Body)
			if c.typ != "" {
				require.Equal(t, c.typ, got.Type, got.Body)
			}
		})
	}
}

// TestBedrockPipeline_QuotaOrder pins where metering sits: runtime checks the
// request rate before it reads the body, agent-runtime only for
// RetrieveAndGenerate, and nothing else meters.
func TestBedrockPipeline_QuotaOrder(t *testing.T) {
	limited := func(c *bedrockPipelineEnv) {
		c.Deps.Quota = admissionquota.New(admissionquota.Limits{RequestsPerMinuteEnabled: true, RequestsPerMinute: 1}, nil)
	}

	t.Run("runtime throttles before the body is read", func(t *testing.T) {
		app := bedrockPipelineApp(t, limited, allowBedrock())
		path := bedrockPipelineModelPath + "/converse"
		first := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", path, []byte("{")))
		require.Equal(t, "ValidationException", first.Type, first.Body)
		second := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", path, []byte("{")))
		require.Equal(t, "ThrottlingException", second.Type, second.Body)
		require.Equal(t, http.StatusTooManyRequests, second.Status)
	})

	t.Run("runtime policy deny precedes metering", func(t *testing.T) {
		app := bedrockPipelineApp(t, limited)
		path := bedrockPipelineModelPath + "/converse"
		for range 3 {
			got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", path, []byte("{")))
			require.Equal(t, "AccessDeniedException", got.Type, got.Body)
		}
	})

	t.Run("agent-runtime Retrieve is not metered", func(t *testing.T) {
		app := bedrockPipelineApp(t, limited, allowBedrock())
		for range 3 {
			got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", "/knowledgebases/kb1/retrieve", retrieveBody))
			require.Equal(t, "ResourceNotFoundException", got.Type, got.Body)
		}
	})

	t.Run("agent-runtime RetrieveAndGenerate is metered after the gate", func(t *testing.T) {
		app := bedrockPipelineApp(t, limited, allowBedrock())
		body := []byte(`{"input":{"text":"q"},"retrieveAndGenerateConfiguration":{"type":"KNOWLEDGE_BASE","knowledgeBaseConfiguration":{"knowledgeBaseId":"kb1","modelArn":"arn:aws:bedrock:us-east-1::foundation-model/` + bedrockPipelineLlamaModel + `"}}}`)
		first := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", "/retrieveAndGenerate", body))
		require.NotEqual(t, "ThrottlingException", first.Type, first.Body)
		second := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", "/retrieveAndGenerate", body))
		require.Equal(t, "ThrottlingException", second.Type, second.Body)
	})

	t.Run("bedrock control plane is not metered", func(t *testing.T) {
		app := bedrockPipelineApp(t, limited, allowBedrock())
		for range 3 {
			got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/guardrails", nil))
			require.Equal(t, http.StatusOK, got.Status, got.Body)
		}
	})
}

// TestBedrockPipeline_QueryFold pins that a body-less request's singular query
// parameters become the JSON body, and that a real body is never shadowed.
func TestBedrockPipeline_QueryFold(t *testing.T) {
	app := bedrockPipelineApp(t, nil, allowBedrock())
	create := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", "/guardrails",
		[]byte(`{"name":"g","blockedInputMessaging":"x","blockedOutputsMessaging":"y"}`)))
	require.Equal(t, http.StatusOK, create.Status, create.Body)
	var created struct {
		GuardrailID string `json:"guardrailId"`
	}
	require.NoError(t, json.Unmarshal([]byte(create.Body), &created))
	id := created.GuardrailID
	require.NotEmpty(t, id)

	ver := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", "/guardrails/"+id, []byte(`{}`)))
	require.Equal(t, http.StatusOK, ver.Status, ver.Body)

	draft := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/guardrails/"+id, nil))
	require.Equal(t, http.StatusOK, draft.Status, draft.Body)
	assert.Contains(t, draft.Body, `"version":"DRAFT"`)

	folded := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/guardrails/"+id+"?guardrailVersion=1", nil))
	require.Equal(t, http.StatusOK, folded.Status, folded.Body)
	assert.Contains(t, folded.Body, `"version":"1"`)
}

// TestBedrockPipeline_NilCapabilityFallbacks pins what an unset capability
// looks like through the pipeline.
func TestBedrockPipeline_NilCapabilityFallbacks(t *testing.T) {
	t.Run("no access store hides the catalog", func(t *testing.T) {
		app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Access = nil }, allowBedrock())
		got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/foundation-models", nil))
		require.Equal(t, http.StatusOK, got.Status, got.Body)
		assert.NotContains(t, got.Body, bedrockPipelineLlamaModel)
	})

	t.Run("no access store refuses runtime", func(t *testing.T) {
		app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Access = nil }, allowBedrock())
		got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", bedrockPipelineModelPath+"/converse",
			[]byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)))
		require.Equal(t, http.StatusForbidden, got.Status, got.Body)
		require.Equal(t, "AccessDeniedException", got.Type)
	})

	t.Run("no guardrail store errors without panicking", func(t *testing.T) {
		app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Guardrails = nil }, allowBedrock())
		got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/guardrails", nil))
		require.Equal(t, http.StatusInternalServerError, got.Status, got.Body)
		require.Equal(t, "InternalErrorException", got.Type)
	})

	t.Run("no logging store errors without panicking", func(t *testing.T) {
		app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Logging = nil }, allowBedrock())
		got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/logging/modelinvocations", nil))
		require.Equal(t, http.StatusInternalServerError, got.Status, got.Body)
		require.Equal(t, "InternalErrorException", got.Type)
	})

	t.Run("no provisioned store errors without panicking", func(t *testing.T) {
		app := bedrockPipelineApp(t, func(c *bedrockPipelineEnv) { c.Deps.Provisioned = nil }, allowBedrock())
		got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "GET", "/provisioned-model-throughputs", nil))
		require.Equal(t, http.StatusInternalServerError, got.Status, got.Body)
		require.Equal(t, "InternalErrorException", got.Type)
	})
}

// TestBedrockPipeline_ActionResolution pins that an authorized request reaches
// its handler for one action of each service, so a signing-scope or path
// change cannot silently drop a service.
func TestBedrockPipeline_ActionResolution(t *testing.T) {
	cases := []struct {
		name   string
		action string
		method string
		path   string
		body   string
		status int
	}{
		{"ListFoundationModels", "bedrock:ListFoundationModels", "GET", "/foundation-models", "", 200},
		{"ListGuardrails", "bedrock:ListGuardrails", "GET", "/guardrails", "", 200},
		{"ListKnowledgeBases", "bedrock:ListKnowledgeBases", "POST", "/knowledgebases/", "{}", 200},
		{"GetKnowledgeBase", "bedrock:GetKnowledgeBase", "GET", "/knowledgebases/kb1", "", 404},
		{"Retrieve", "bedrock:Retrieve", "POST", "/knowledgebases/kb1/retrieve", `{"retrievalQuery":{"text":"q"}}`, 404},
		{"Converse", "bedrock:Converse", "POST", bedrockPipelineModelPath + "/converse", "{", 400},
	}
	for _, c := range cases {
		t.Run(c.name+" allowed only by its own action", func(t *testing.T) {
			var body []byte
			if c.body != "" {
				body = []byte(c.body)
			}
			allowed := bedrockPipelineApp(t, nil, statement("Allow", c.action, "*"))
			got := probeBedrock(t, allowed, signBedrockRequest(t, "bedrock", c.method, c.path, body))
			require.Equal(t, c.status, got.Status, got.Body)

			other := bedrockPipelineApp(t, nil, statement("Allow", "bedrock:NoSuchAction", "*"))
			denied := probeBedrock(t, other, signBedrockRequest(t, "bedrock", c.method, c.path, body))
			require.Equal(t, http.StatusForbidden, denied.Status, denied.Body)
			require.Equal(t, "AccessDeniedException", denied.Type)
		})
	}
}

// TestBedrockPipeline_BodyTooLarge pins the oversize-body answer on a
// signed bedrock request.
func TestBedrockPipeline_BodyTooLarge(t *testing.T) {
	app := bedrockPipelineApp(t, nil, allowBedrock())
	big := []byte(`{"name":"` + strings.Repeat("a", 11<<20) + `"}`)
	got := probeBedrock(t, app, signBedrockRequest(t, "bedrock", "POST", "/guardrails", big))
	require.Equal(t, http.StatusRequestEntityTooLarge, got.Status, got.Body[:min(len(got.Body), 200)])
}
