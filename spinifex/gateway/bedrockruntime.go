package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
)

// InvokeModel/InvokeModelWithResponseStream carry their guardrail selection
// as headers rather than body fields (unlike Converse's GuardrailConfig).
const (
	bedrockGuardrailIdentifierHeader = "X-Amzn-Bedrock-Guardrailidentifier"
	bedrockGuardrailVersionHeader    = "X-Amzn-Bedrock-Guardrailversion"
)

// bedrockRuntimeRoute maps one HTTP method + chi path pattern to an AWS action and handler.
type bedrockRuntimeRoute = rest.Route[bedrockRuntimeRouteHandler]

// bedrockRuntimeRouteHandler invokes a per-action bedrock-runtime (data-plane)
// gateway function. params holds the path params, PathUnescape'd.
// resolver is gw.bedrockResolver() (credential store or no-op); endpoints is
// gw.bedrockEndpointResolver() over the configured pinned self-host
// endpoints; recorder is gw.bedrockRecorder() (invocation recorder or no-op);
// access is gw.bedrockAccessResolver() (grant store or deny-all); provisioned
// is gw.bedrockProvisionedStore(), consulted when modelId is a PT ARN;
// guardrails is gw.bedrockGuardrailStore(); embedder is gw.bedrockEmbedder(),
// driving guardrail topicPolicy's semantic match.
type bedrockRuntimeRouteHandler func(ctx context.Context, accountID string, params []string, body []byte, resolver gateway_bedrock.CredentialResolver, endpoints gateway_bedrock.EndpointResolver, recorder gateway_bedrock.Recorder, access gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore, embedder gateway_bedrock.Embedder) (any, error)

// bedrockRuntimeRoutes is the dispatch table. InvokeModel has no handler
// function here: BedrockRuntime_Request special-cases its action to bypass
// the JSON-marshaling dispatch below, since its response is raw bytes.
var bedrockRuntimeRoutes = []bedrockRuntimeRoute{
	{Method: "POST", Pattern: "/model/{modelId}/converse", Action: "Converse",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, resolver gateway_bedrock.CredentialResolver, endpoints gateway_bedrock.EndpointResolver, recorder gateway_bedrock.Recorder, access gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore, embedder gateway_bedrock.Embedder) (any, error) {
			input := new(bedrockruntime.ConverseInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return gateway_bedrock.Converse(ctx, acct, p[0], input, resolver, endpoints, recorder, access, provisioned, guardrails, embedder)
		}},
	{Method: "POST", Pattern: "/model/{modelId}/invoke", Action: "InvokeModel", Handler: nil},
	{Method: "POST", Pattern: "/model/{modelId}/converse-stream", Action: "ConverseStream", Handler: nil},
	{Method: "POST", Pattern: "/model/{modelId}/invoke-with-response-stream", Action: "InvokeModelWithResponseStream", Handler: nil},
	{Method: "POST", Pattern: "/guardrail/{guardrailIdentifier}/version/{guardrailVersion}/apply", Action: "ApplyGuardrail",
		Handler: func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ gateway_bedrock.EndpointResolver, _ gateway_bedrock.Recorder, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore, embedder gateway_bedrock.Embedder) (any, error) {
			input := new(bedrockruntime.ApplyGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			input.GuardrailVersion = aws.String(p[1])
			return gateway_bedrock.ApplyGuardrail(ctx, acct, guardrails, embedder, input)
		}},
}

// bedrockRuntimeRouter matches an escaped request path against bedrockRuntimeRoutes.
var bedrockRuntimeRouter = rest.NewRouter("bedrock-runtime", bedrockRuntimeRoutes)

// BedrockRuntime_Request dispatches bedrock-runtime (data-plane) REST-JSON
// requests: resolves method+path to an action, reads the body, calls the
// handler, and serialises the output as JSON.
func (gw *GatewayConfig) BedrockRuntime_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := bedrockRuntimeRouter.Lookup(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "bedrock-runtime: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from it.
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "BedrockRuntime_Request: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	// Every bedrock-runtime action names its model or guardrail in the path, so
	// the gate resolves before the body is read at all.
	resources, err := gateway_bedrock.ResourceARNs("bedrock-runtime", action, gw.Region, accountID, params, nil)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "bedrock-runtime", action, resources); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Ochre metering. RPM is a local, in-memory check —
	// cheap enough to run before the body is even read. The token cap reads
	// the stream-fed usage counter, so it goes second; both are no-ops
	// (nil-safe on gw.Quota) unless their dimension is explicitly enabled.
	if err := gw.Quota.CheckBedrockRPM(accountID); err != nil {
		return err
	}
	if err := gw.Quota.CheckBedrockTokens(r.Context(), accountID); err != nil {
		return err
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "BedrockRuntime_Request: failed to read body", "err", err)
		return err
	}

	// InvokeModel returns provider-native bytes, not a struct WriteJSONResponse
	// could marshal, so it writes its own response body directly.
	if action == "InvokeModel" {
		guardrailIdent := r.Header.Get(bedrockGuardrailIdentifierHeader)
		guardrailVersion := r.Header.Get(bedrockGuardrailVersionHeader)
		respBody, contentType, err := gateway_bedrock.InvokeModel(r.Context(), accountID, params[0], body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), guardrailIdent, guardrailVersion, gw.bedrockGuardrailStore(), gw.bedrockEmbedder())
		if err != nil {
			return err
		}
		gateway_bedrock.WriteRawResponse(w, respBody, contentType)
		return nil
	}

	// ConverseStream and InvokeModelWithResponseStream own w directly and
	// write framed event-stream bytes as they arrive, rather than one
	// buffered struct/body WriteJSONResponse/WriteRawResponse could send in
	// one shot. Each returns an error ONLY for a pre-first-frame failure
	// (-> ErrorHandler); once streaming starts they always return nil,
	// surfacing any further failure as an in-band exception event.
	if action == "ConverseStream" {
		return gateway_bedrock.ConverseStream(r.Context(), w, accountID, params[0], body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), gw.bedrockGuardrailStore(), gw.bedrockEmbedder())
	}
	if action == "InvokeModelWithResponseStream" {
		guardrailIdent := r.Header.Get(bedrockGuardrailIdentifierHeader)
		guardrailVersion := r.Header.Get(bedrockGuardrailVersionHeader)
		return gateway_bedrock.InvokeModelWithResponseStream(r.Context(), w, accountID, params[0], body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), r.Header.Get("Content-Type"), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), guardrailIdent, guardrailVersion, gw.bedrockGuardrailStore(), gw.bedrockEmbedder())
	}

	output, err := handler(r.Context(), accountID, params, body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), gw.bedrockGuardrailStore(), gw.bedrockEmbedder())
	if err != nil {
		return err
	}

	gateway_bedrock.WriteJSONResponse(w, output)
	return nil
}
