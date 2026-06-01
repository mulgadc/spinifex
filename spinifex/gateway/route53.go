package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_route53 "github.com/mulgadc/spinifex/spinifex/gateway/route53"
)

// Route53PathPrefix is the AWS Route53 API version prefix. Every wire
// path arrives as /2013-04-01/... and is stripped before the route
// table matches the remaining sub-path.
const Route53PathPrefix = "/2013-04-01"

// GenerateRoute53ErrorResponse exposes the Route53 REST/XML error
// envelope to the shared gateway plumbing (writeClusterUnavailable,
// writeThrottleError, ErrorHandler).
func GenerateRoute53ErrorResponse(code, message, requestID string) []byte {
	return gateway_route53.GenerateErrorResponse(code, message, requestID)
}

// route53Route encodes one HTTP method + path-regex → AWS action
// dispatch. The pattern matches against the path with Route53PathPrefix
// stripped (e.g. /hostedzone/Z123, not /2013-04-01/hostedzone/Z123).
type route53Route struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler route53RouteHandler
}

// route53RouteHandler invokes the per-action gateway function. It
// receives the path-capture values in declaration order, the request
// body, the parent gateway config, the caller's accountID, and the URL
// query values (for ListResourceRecordSets pagination params).
type route53RouteHandler func(gw *GatewayConfig, accountID string, params []string, body []byte, query map[string][]string) (any, error)

// route53Routes is the dispatch table. Order matters: more-specific
// paths come before bare-resource matches with the same prefix
// (e.g. /hostedzone/{Id}/rrset before /hostedzone/{Id}).
var route53Routes = []route53Route{
	// Hosted zones
	{"POST", regexp.MustCompile(`^/hostedzone$`), "CreateHostedZone",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.CreateHostedZone(gw.NATSConn, acct, b)
		}},
	{"GET", regexp.MustCompile(`^/hostedzone$`), "ListHostedZones",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.ListHostedZones(gw.NATSConn, acct)
		}},

	// Resource record sets — must precede the bare /hostedzone/{Id}
	// matches so the deeper path wins.
	{"POST", regexp.MustCompile(`^/hostedzone/([^/]+)/rrset/?$`), "ChangeResourceRecordSets",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.ChangeResourceRecordSets(gw.NATSConn, acct, p[0], b)
		}},
	{"GET", regexp.MustCompile(`^/hostedzone/([^/]+)/rrset/?$`), "ListResourceRecordSets",
		func(gw *GatewayConfig, acct string, p []string, b []byte, q map[string][]string) (any, error) {
			return gateway_route53.ListResourceRecordSets(gw.NATSConn, acct, p[0], q)
		}},

	// Hosted zone (single)
	{"GET", regexp.MustCompile(`^/hostedzone/([^/]+)$`), "GetHostedZone",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.GetHostedZone(gw.NATSConn, acct, p[0])
		}},
	{"POST", regexp.MustCompile(`^/hostedzone/([^/]+)$`), "UpdateHostedZoneComment",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.UpdateHostedZoneComment(gw.NATSConn, acct, p[0], b)
		}},
	{"DELETE", regexp.MustCompile(`^/hostedzone/([^/]+)$`), "DeleteHostedZone",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.DeleteHostedZone(gw.NATSConn, acct, p[0])
		}},

	// Change tracking
	{"GET", regexp.MustCompile(`^/change/([^/]+)$`), "GetChange",
		func(gw *GatewayConfig, acct string, p []string, b []byte, _ map[string][]string) (any, error) {
			return gateway_route53.GetChange(gw.NATSConn, acct, p[0])
		}},
}

// lookupRoute53Action walks route53Routes in declaration order. Returns
// the matched action name + captured path params + handler, or
// ("", nil, nil, false) on no-match.
func lookupRoute53Action(method, path string) (string, []string, route53RouteHandler, bool) {
	stripped := strings.TrimPrefix(path, Route53PathPrefix)
	if stripped == path {
		return "", nil, nil, false
	}
	for _, route := range route53Routes {
		if route.method != method {
			continue
		}
		m := route.pattern.FindStringSubmatch(stripped)
		if m == nil {
			continue
		}
		var params []string
		if len(m) > 1 {
			params = m[1:]
		}
		return route.action, params, route.handler, true
	}
	return "", nil, nil, false
}

// Route53_Request is the chi catch-all dispatcher for Route53 REST/XML
// requests. Parses method+path, resolves to an AWS action, reads the
// body, invokes the per-action handler, and serialises the typed
// output as XML. Errors are returned as plain awserrors codes; the
// caller (gateway.Request) routes them through the shared ErrorHandler.
func (gw *GatewayConfig) Route53_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupRoute53Action(r.Method, r.URL.Path)
	if !ok {
		slog.Debug("Route53: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "route53", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("Route53_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := gateway_route53.ReadBody(r)
	if err != nil {
		slog.Error("Route53_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidInput)
	}

	output, err := handler(gw, accountID, params, body, r.URL.Query())
	if err != nil {
		return err
	}

	xmlBody, err := gateway_route53.MarshalResponseXML(output)
	if err != nil {
		slog.Error("Route53_Request: failed to marshal response", "action", action, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	w.Header().Set("Content-Type", gateway_route53.XMLContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlBody); err != nil {
		slog.Error("Route53_Request: failed to write response", "err", err)
	}
	return nil
}
