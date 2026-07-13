package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

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
// fallback.
type bedrockRouteHandler func(ctx context.Context, accountID string, params []string, body []byte, resolver gateway_bedrock.CredentialResolver) (any, error)

// bedrockRoutes is the dispatch table. More-specific paths must precede
// less-specific ones with the same prefix so the regex matcher picks the
// deeper route first.
var bedrockRoutes = []bedrockRoute{
	{"GET", regexp.MustCompile(`^/foundation-models$`), "ListFoundationModels",
		func(ctx context.Context, acct string, p []string, b []byte, resolver gateway_bedrock.CredentialResolver) (any, error) {
			return gateway_bedrock.ListFoundationModels(ctx, acct, resolver, new(bedrock.ListFoundationModelsInput))
		}},
	{"GET", regexp.MustCompile(`^/foundation-models/([^/]+)$`), "GetFoundationModel",
		func(ctx context.Context, acct string, p []string, b []byte, _ gateway_bedrock.CredentialResolver) (any, error) {
			return gateway_bedrock.GetFoundationModel(ctx, acct, p[0])
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

	if err := gw.checkPolicy(r, "bedrock", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "Bedrock_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "Bedrock_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	output, err := handler(r.Context(), accountID, params, body, gw.bedrockResolver())
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

// bedrockEndpointResolver returns an EndpointResolver over the configured
// pinned self-host endpoints (gw.BedrockEndpoints). A nil/empty map resolves
// nothing, so self-host models return ModelNotReady until configured.
func (gw *GatewayConfig) bedrockEndpointResolver() gateway_bedrock.EndpointResolver {
	return gateway_bedrock.NewStaticEndpointResolver(gw.BedrockEndpoints)
}
