package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
)

// bedrockRoute maps one HTTP method + path regex to an AWS action and handler.
type bedrockRoute struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler bedrockRouteHandler
}

// bedrockRouteHandler invokes a per-action bedrock (control-plane) gateway
// function. params holds the regex capture groups, PathUnescape'd. resolver
// is gw.bedrockResolver(): the configured credential store, or a no-op
// fallback. loggingStore is gw.bedrockLoggingConfigStore(). access is
// gw.bedrockAccessResolver(): the configured grant store, or a deny-all
// fallback. provisioned is gw.bedrockProvisionedStore(). guardrails is
// gw.bedrockGuardrailStore().
type bedrockRouteHandler func(ctx context.Context, accountID string, params []string, body []byte, resolver gateway_bedrock.CredentialResolver, loggingStore *gateway_bedrock.LoggingConfigStore, access gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error)

// bedrockRoutes is the dispatch table. More-specific paths must precede
// less-specific ones with the same prefix so the regex matcher picks the
// deeper route first.
var bedrockRoutes = []bedrockRoute{
	{"GET", regexp.MustCompile(`^/foundation-models$`), "ListFoundationModels",
		func(ctx context.Context, acct string, p []string, b []byte, resolver gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, access gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.ListFoundationModels(ctx, acct, resolver, access, new(bedrock.ListFoundationModelsInput))
		}},
	{"GET", regexp.MustCompile(`^/foundation-models/([^/]+)$`), "GetFoundationModel",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, access gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.GetFoundationModel(ctx, acct, p[0], access)
		}},
	{"PUT", regexp.MustCompile(`^/logging/modelinvocations$`), "PutModelInvocationLoggingConfiguration",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, store *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.PutModelInvocationLoggingConfigurationInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return gateway_bedrock.PutModelInvocationLoggingConfiguration(ctx, acct, store, input)
		}},
	{"GET", regexp.MustCompile(`^/logging/modelinvocations$`), "GetModelInvocationLoggingConfiguration",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, store *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.GetModelInvocationLoggingConfiguration(ctx, acct, store, new(bedrock.GetModelInvocationLoggingConfigurationInput))
		}},
	{"DELETE", regexp.MustCompile(`^/logging/modelinvocations$`), "DeleteModelInvocationLoggingConfiguration",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, store *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.DeleteModelInvocationLoggingConfiguration(ctx, acct, store, new(bedrock.DeleteModelInvocationLoggingConfigurationInput))
		}},
	{"POST", regexp.MustCompile(`^/provisioned-model-throughput$`), "CreateProvisionedModelThroughput",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.CreateProvisionedModelThroughputInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return gateway_bedrock.CreateProvisionedModelThroughput(ctx, acct, provisioned, input)
		}},
	{"GET", regexp.MustCompile(`^/provisioned-model-throughput/([^/]+)$`), "GetProvisionedModelThroughput",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.GetProvisionedModelThroughput(ctx, acct, provisioned, &bedrock.GetProvisionedModelThroughputInput{ProvisionedModelId: aws.String(p[0])})
		}},
	{"GET", regexp.MustCompile(`^/provisioned-model-throughputs$`), "ListProvisionedModelThroughputs",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.ListProvisionedModelThroughputs(ctx, acct, provisioned, new(bedrock.ListProvisionedModelThroughputsInput))
		}},
	{"PATCH", regexp.MustCompile(`^/provisioned-model-throughput/([^/]+)$`), "UpdateProvisionedModelThroughput",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.UpdateProvisionedModelThroughputInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.ProvisionedModelId = aws.String(p[0])
			return gateway_bedrock.UpdateProvisionedModelThroughput(ctx, acct, provisioned, input)
		}},
	{"DELETE", regexp.MustCompile(`^/provisioned-model-throughput/([^/]+)$`), "DeleteProvisionedModelThroughput",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, provisioned *gateway_bedrock.ProvisionedStore, _ *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.DeleteProvisionedModelThroughput(ctx, acct, provisioned, &bedrock.DeleteProvisionedModelThroughputInput{ProvisionedModelId: aws.String(p[0])})
		}},
	{"POST", regexp.MustCompile(`^/guardrails$`), "CreateGuardrail",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.CreateGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return gateway_bedrock.CreateGuardrail(ctx, acct, guardrails, input)
		}},
	{"GET", regexp.MustCompile(`^/guardrails/([^/]+)$`), "GetGuardrail",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.GetGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return gateway_bedrock.GetGuardrail(ctx, acct, guardrails, input)
		}},
	{"GET", regexp.MustCompile(`^/guardrails$`), "ListGuardrails",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			return gateway_bedrock.ListGuardrails(ctx, acct, guardrails, new(bedrock.ListGuardrailsInput))
		}},
	{"PUT", regexp.MustCompile(`^/guardrails/([^/]+)$`), "UpdateGuardrail",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.UpdateGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return gateway_bedrock.UpdateGuardrail(ctx, acct, guardrails, input)
		}},
	{"DELETE", regexp.MustCompile(`^/guardrails/([^/]+)$`), "DeleteGuardrail",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.DeleteGuardrailInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return gateway_bedrock.DeleteGuardrail(ctx, acct, guardrails, input)
		}},
	{"POST", regexp.MustCompile(`^/guardrails/([^/]+)$`), "CreateGuardrailVersion",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver, _ *gateway_bedrock.LoggingConfigStore, _ gateway_bedrock.AccessResolver, _ *gateway_bedrock.ProvisionedStore, guardrails *gateway_bedrock.GuardrailStore) (any, error) {
			input := new(bedrock.CreateGuardrailVersionInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.GuardrailIdentifier = aws.String(p[0])
			return gateway_bedrock.CreateGuardrailVersion(ctx, acct, guardrails, input)
		}},
}

// lookupBedrockAction matches method+path against bedrockRoutes, returning the
// action, path params, and handler, or ("", nil, nil, false) on no match.
// path must be r.URL.EscapedPath(): captured params are PathUnescape'd before
// returning, mirroring lookupEKSAction.
func lookupBedrockAction(method, path string) (string, []string, bedrockRouteHandler, bool) {
	for _, route := range bedrockRoutes {
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
					slog.Debug("bedrock: bad percent-encoding in path param", "param", raw, "err", err)
					decoded = raw
				}
				params = append(params, decoded)
			}
		}
		return route.action, params, route.handler, true
	}
	return "", nil, nil, false
}

// Bedrock_Request dispatches bedrock (control-plane) REST-JSON requests:
// resolves method+path to an action, reads the body, calls the handler, and
// serialises the output as JSON.
func (gw *GatewayConfig) Bedrock_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupBedrockAction(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "bedrock: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from it.
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "Bedrock_Request: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "Bedrock_Request: failed to read body", "err", err)
		return err
	}

	// Some REST-JSON actions carry their non-path inputs as singular query
	// params with an empty body (e.g. GetGuardrail's guardrailVersion arrives
	// as GET /guardrails/{id}?guardrailVersion=1). Only folds when the body is
	// empty so it never shadows a real payload, mirroring lookupEKSAction's
	// own query fold for its (repeated-value) tagKeys case.
	if len(body) == 0 {
		if q := r.URL.Query(); len(q) > 0 {
			flat := make(map[string]string, len(q))
			for k, v := range q {
				if len(v) > 0 {
					flat[k] = v[0]
				}
			}
			if qb, err := json.Marshal(flat); err == nil {
				body = qb
			}
		}
	}

	// After the body read and the query fold: CreateProvisionedModelThroughput
	// names the foundation model it commits there rather than in the path.
	resources, err := gateway_bedrock.ResourceARNs("bedrock", action, gw.Region, accountID, params, body)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "bedrock", action, resources); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	output, err := handler(r.Context(), accountID, params, body, gw.bedrockResolver(), gw.bedrockLoggingConfigStore(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), gw.bedrockGuardrailStore())
	if err != nil {
		return err
	}

	gateway_bedrock.WriteJSONResponse(w, output)
	return nil
}

// bedrockResolver returns gw.BedrockCredentials as a CredentialResolver, or
// the no-op fallback when no credential store is configured.
func (gw *GatewayConfig) bedrockResolver() gateway_bedrock.CredentialResolver {
	if gw.BedrockCredentials != nil {
		return gw.BedrockCredentials
	}
	return gateway_bedrock.NoopCredentialResolver
}

// bedrockLoggingConfigStore returns gw.BedrockLoggingConfig, or a store
// backed by no JetStream client when unconfigured. Reads/writes then fail
// with an error (no JetStream to open a KV bucket against) rather than
// panicking, which is acceptable for unit tests of unrelated routes that
// never reach a logging-config handler.
func (gw *GatewayConfig) bedrockLoggingConfigStore() *gateway_bedrock.LoggingConfigStore {
	if gw.BedrockLoggingConfig != nil {
		return gw.BedrockLoggingConfig
	}
	return gateway_bedrock.NewLoggingConfigStore(nil, 1)
}

// bedrockRecorder returns gw.BedrockRecorder, or the no-op fallback when no
// invocation recorder is configured.
func (gw *GatewayConfig) bedrockRecorder() gateway_bedrock.Recorder {
	if gw.BedrockRecorder != nil {
		return gw.BedrockRecorder
	}
	return gateway_bedrock.NoopRecorder
}

// bedrockAccessResolver returns gw.BedrockAccess as an AccessResolver, or the
// deny-all fallback when no grant store is configured. Model access is
// deny-by-default, so an unconfigured gateway advertises and serves no models
// rather than all of them.
func (gw *GatewayConfig) bedrockAccessResolver() gateway_bedrock.AccessResolver {
	if gw.BedrockAccess != nil {
		return gw.BedrockAccess
	}
	return gateway_bedrock.DenyAllAccessResolver
}

// bedrockProvisionedStore returns gw.BedrockProvisioned, or a store backed by
// no JetStream client when unconfigured. Reads/writes then fail with an error
// (no JetStream to open a KV bucket against) rather than panicking, which is
// acceptable for unit tests of unrelated routes that never reach a
// provisioned-throughput handler.
func (gw *GatewayConfig) bedrockProvisionedStore() *gateway_bedrock.ProvisionedStore {
	if gw.BedrockProvisioned != nil {
		return gw.BedrockProvisioned
	}
	return gateway_bedrock.NewProvisionedStore(nil, 1, gw.Region, nil)
}

// bedrockGuardrailStore returns gw.BedrockGuardrails, or a store backed by no
// JetStream client when unconfigured. Reads/writes then fail with an error
// (no JetStream to open a KV bucket against) rather than panicking, which is
// acceptable for unit tests of unrelated routes that never reach a guardrail
// handler.
func (gw *GatewayConfig) bedrockGuardrailStore() *gateway_bedrock.GuardrailStore {
	if gw.BedrockGuardrails != nil {
		return gw.BedrockGuardrails
	}
	return gateway_bedrock.NewGuardrailStore(nil, 1, gw.Region)
}

// bedrockEndpointResolver returns the registry-backed resolver when one is
// configured, which resolves gw.BedrockEndpoints ahead of the registry itself.
// Without it only the pinned endpoints resolve, so a model that was never
// pinned returns ModelNotReady rather than launching.
func (gw *GatewayConfig) bedrockEndpointResolver() gateway_bedrock.EndpointResolver {
	if gw.BedrockEndpointResolver != nil {
		return gw.BedrockEndpointResolver
	}
	return gateway_bedrock.NewStaticEndpointResolver(gw.BedrockEndpoints)
}

// bedrockEmbedder returns gw.BedrockEmbedder, or nil when unconfigured. A nil
// Embedder is a valid value throughout gateway_bedrock's guardrail engine:
// it simply falls back to topicPolicy's literal matcher.
func (gw *GatewayConfig) bedrockEmbedder() gateway_bedrock.Embedder {
	return gw.BedrockEmbedder
}
