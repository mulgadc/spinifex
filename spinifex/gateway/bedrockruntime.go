package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
)

// bedrockRuntimeRoute maps one HTTP method + path regex to an AWS action and handler.
type bedrockRuntimeRoute struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler bedrockRuntimeRouteHandler
}

// bedrockRuntimeRouteHandler invokes a per-action bedrock-runtime (data-plane)
// gateway function. params holds the regex capture groups, PathUnescape'd.
// resolver is gw.bedrockResolver() (credential store or no-op); endpoints is
// gw.bedrockEndpointResolver() over the configured pinned self-host
// endpoints; recorder is gw.bedrockRecorder() (invocation recorder or no-op);
// access is gw.bedrockAccessResolver() (grant store or deny-all); provisioned
// is gw.bedrockProvisionedStore(), consulted when modelId is a PT ARN;
// guardrails is gw.bedrockGuardrailStore().
type bedrockRuntimeRouteHandler func(ctx context.Context, accountID string, params []string, body []byte, resolver gateway_bedrock.CredentialResolver, endpoints gateway_bedrock.EndpointResolver, recorder gateway_bedrock.Recorder, access gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error)

// bedrockRuntimeRoutes is the dispatch table. InvokeModel has no handler
// function here: BedrockRuntime_Request special-cases its action to bypass
// the JSON-marshaling dispatch below, since its response is raw bytes.
var bedrockRuntimeRoutes = []bedrockRuntimeRoute{
	{"POST", regexp.MustCompile(`^/model/([^/]+)/converse$`), "Converse",
		func(ctx context.Context, acct string, p []string, b []byte, resolver gateway_bedrock.CredentialResolver, endpoints gateway_bedrock.EndpointResolver, recorder gateway_bedrock.Recorder, access gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrockruntime.ConverseInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return gateway_bedrock.Converse(ctx, acct, p[0], input, resolver, endpoints, recorder, access, provisioned)
		}},
	{"POST", regexp.MustCompile(`^/model/([^/]+)/invoke$`), "InvokeModel", nil},
	{"POST", regexp.MustCompile(`^/model/([^/]+)/converse-stream$`), "ConverseStream", nil},
	{"POST", regexp.MustCompile(`^/model/([^/]+)/invoke-with-response-stream$`), "InvokeModelWithResponseStream", nil},
	{"POST", regexp.MustCompile(`^/guardrail/([^/]+)/version/([^/]+)/apply$`), "ApplyGuardrail",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ gateway_bedrock.EndpointResolver, _ gateway_bedrock.Recorder, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrockruntime.ApplyGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			input.GuardrailVersion = aws.String(p[1])
			return gateway_bedrock.ApplyGuardrail(ctx, acct, guardrails, input)
		}},
}

// lookupBedrockRuntimeAction matches method+path against bedrockRuntimeRoutes,
// returning the action, path params, and handler, or ("", nil, nil, false) on
// no match. path must be r.URL.EscapedPath(): captured params are
// PathUnescape'd before returning, mirroring lookupEKSAction.
func lookupBedrockRuntimeAction(method, path string) (string, []string, bedrockRuntimeRouteHandler, bool) {
	for _, route := range bedrockRuntimeRoutes {
		if route.method != method {
			continue
		}
		m := route.pattern.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		var params []string
		if len(m) > 1 {
			params = make([]string, 0, len(m)-1)
			for _, raw := range m[1:] {
				decoded, err := url.PathUnescape(raw)
				if err != nil {
					slog.Debug("bedrock-runtime: bad percent-encoding in path param", "param", raw, "err", err)
					decoded = raw
				}
				params = append(params, decoded)
			}
		}
		return route.action, params, route.handler, true
	}
	return "", nil, nil, false
}

// BedrockRuntime_Request dispatches bedrock-runtime (data-plane) REST-JSON
// requests: resolves method+path to an action, reads the body, calls the
// handler, and serialises the output as JSON.
func (gw *GatewayConfig) BedrockRuntime_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupBedrockRuntimeAction(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "bedrock-runtime: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "bedrock-runtime", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "BedrockRuntime_Request: no account ID in auth context")
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "BedrockRuntime_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// InvokeModel returns provider-native bytes, not a struct WriteJSONResponse
	// could marshal, so it writes its own response body directly.
	if action == "InvokeModel" {
		respBody, contentType, err := gateway_bedrock.InvokeModel(r.Context(), accountID, params[0], body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore())
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
		return gateway_bedrock.ConverseStream(r.Context(), w, accountID, params[0], body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore())
	}
	if action == "InvokeModelWithResponseStream" {
		return gateway_bedrock.InvokeModelWithResponseStream(r.Context(), w, accountID, params[0], body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), r.Header.Get("Content-Type"), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore())
	}

	output, err := handler(r.Context(), accountID, params, body, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), gw.bedrockGuardrailStore())
	if err != nil {
		return err
	}

	gateway_bedrock.WriteJSONResponse(w, output)
	return nil
}
