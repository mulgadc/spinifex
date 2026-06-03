package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
)

// GenerateEKSErrorResponse exposes the EKS REST-JSON error envelope to the
// shared gateway plumbing (writeClusterUnavailable, writeThrottleError,
// ErrorHandler). The body is JSON: {"__type":"<code>Exception","message":"<msg>"}.
func GenerateEKSErrorResponse(code, message, _ string) []byte {
	return gateway_eks.GenerateEKSErrorResponse(code, message)
}

// eksRoute encodes one HTTP method + path-regex → AWS action dispatch.
// Path params are captured by the regex and passed to the per-route handler
// via the params slice (named-capture index order, not by name).
type eksRoute struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler eksRouteHandler
}

// eksRouteHandler invokes the per-action gateway function. It receives the
// path-capture values in declaration order, the request body, the parent
// gateway config (NATS conn + ServiceURL), the caller's accountID, and the
// caller's resolved IAM principal ARN (used by CreateCluster for the
// bootstrap-creator-admin AccessEntry; ignored by the rest). It returns the
// response payload object (already a typed *eks.<X>Output) or an error whose
// Error() is an awserrors code.
type eksRouteHandler func(gw *GatewayConfig, accountID, callerARN string, params []string, body []byte) (any, error)

// eksRoutes is the dispatch table. Order matters: more-specific paths must
// come before less-specific ones with the same prefix (e.g. the
// /access-policies leaf under /clusters/{name}/access-entries/{arn}/...
// before the bare /clusters/{name}/access-entries/{arn} entry).
var eksRoutes = []eksRoute{
	// Cluster
	{"POST", regexp.MustCompile(`^/clusters$`), "CreateCluster",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateCluster(gw.NATSConn, acct, callerARN, b)
		}},
	{"GET", regexp.MustCompile(`^/clusters$`), "ListClusters",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListClusters(gw.NATSConn, acct)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/update-config$`), "UpdateClusterConfig",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateClusterConfig(gw.NATSConn, acct, p[0], b)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/update-version$`), "UpdateClusterVersion",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateClusterVersion(gw.NATSConn, acct, p[0], b)
		}},

	// Nodegroup
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/node-groups$`), "CreateNodegroup",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateNodegroup(gw.NATSConn, acct, p[0], b)
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/node-groups$`), "ListNodegroups",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListNodegroups(gw.NATSConn, acct, p[0])
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/node-groups/([^/]+)/update-config$`), "UpdateNodegroupConfig",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateNodegroupConfig(gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/node-groups/([^/]+)/update-version$`), "UpdateNodegroupVersion",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateNodegroupVersion(gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/node-groups/([^/]+)$`), "DescribeNodegroup",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeNodegroup(gw.NATSConn, acct, p[0], p[1])
		}},
	{"DELETE", regexp.MustCompile(`^/clusters/([^/]+)/node-groups/([^/]+)$`), "DeleteNodegroup",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteNodegroup(gw.NATSConn, acct, p[0], p[1])
		}},

	// AccessEntry / AccessPolicy
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/access-entries$`), "CreateAccessEntry",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateAccessEntry(gw.NATSConn, acct, p[0], b)
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/access-entries$`), "ListAccessEntries",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAccessEntries(gw.NATSConn, acct, p[0])
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/access-entries/([^/]+)/access-policies$`), "AssociateAccessPolicy",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.AssociateAccessPolicy(gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"DELETE", regexp.MustCompile(`^/clusters/([^/]+)/access-entries/([^/]+)/access-policies$`), "DisassociateAccessPolicy",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DisassociateAccessPolicy(gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/access-entries/([^/]+)/access-policies$`), "ListAssociatedAccessPolicies",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAssociatedAccessPolicies(gw.NATSConn, acct, p[0], p[1])
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/access-entries/([^/]+)$`), "DescribeAccessEntry",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAccessEntry(gw.NATSConn, acct, p[0], p[1])
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/access-entries/([^/]+)$`), "UpdateAccessEntry",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateAccessEntry(gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"DELETE", regexp.MustCompile(`^/clusters/([^/]+)/access-entries/([^/]+)$`), "DeleteAccessEntry",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteAccessEntry(gw.NATSConn, acct, p[0], p[1])
		}},
	{"GET", regexp.MustCompile(`^/access-policies$`), "ListAccessPolicies",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAccessPolicies(gw.NATSConn, acct)
		}},

	// Addons
	{"GET", regexp.MustCompile(`^/addons/supported-versions$`), "DescribeAddonVersions",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAddonVersions(gw.NATSConn, acct)
		}},
	{"GET", regexp.MustCompile(`^/addons$`), "ListAddons",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAddons(gw.NATSConn, acct)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/addons$`), "CreateAddon",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateAddon(gw.NATSConn, acct, p[0], b)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/addons/([^/]+)/update$`), "UpdateAddon",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateAddon(gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/addons/([^/]+)$`), "DescribeAddon",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAddon(gw.NATSConn, acct, p[0], p[1])
		}},
	{"DELETE", regexp.MustCompile(`^/clusters/([^/]+)/addons/([^/]+)$`), "DeleteAddon",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteAddon(gw.NATSConn, acct, p[0], p[1])
		}},

	// OIDC identity-provider configs
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/identity-provider-configs/associate$`), "AssociateIdentityProviderConfig",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.AssociateIdentityProviderConfig(gw.NATSConn, acct, p[0], b)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/identity-provider-configs/describe$`), "DescribeIdentityProviderConfig",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeIdentityProviderConfig(gw.NATSConn, acct, p[0], b)
		}},
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/identity-provider-configs/disassociate$`), "DisassociateIdentityProviderConfig",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DisassociateIdentityProviderConfig(gw.NATSConn, acct, p[0], b)
		}},
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/identity-provider-configs$`), "ListIdentityProviderConfigs",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListIdentityProviderConfigs(gw.NATSConn, acct, p[0])
		}},

	// Cluster CRUD on a specific name — listed after the more-specific
	// /clusters/{name}/... routes so the regexp matcher prefers the deeper
	// matches first when iterating in declaration order.
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)$`), "DescribeCluster",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeCluster(gw.NATSConn, acct, p[0])
		}},
	{"DELETE", regexp.MustCompile(`^/clusters/([^/]+)$`), "DeleteCluster",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteCluster(gw.NATSConn, acct, p[0])
		}},

	// Tags
	{"POST", regexp.MustCompile(`^/tags/(.+)$`), "TagResource",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.TagResource(gw.NATSConn, acct, p[0], b)
		}},
	{"DELETE", regexp.MustCompile(`^/tags/(.+)$`), "UntagResource",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UntagResource(gw.NATSConn, acct, p[0], b)
		}},
	{"GET", regexp.MustCompile(`^/tags/(.+)$`), "ListTagsForResource",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListTagsForResource(gw.NATSConn, acct, p[0])
		}},
}

// lookupEKSAction walks eksRoutes in declaration order, returning the matched
// route's action name + path params, or ("", nil, false) when nothing matches.
// Used by EKS_Request and unit tests to verify routing.
func lookupEKSAction(method, path string) (string, []string, eksRouteHandler, bool) {
	for _, route := range eksRoutes {
		if route.method != method {
			continue
		}
		m := route.pattern.FindStringSubmatch(path)
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

// EKS_Request is the chi catch-all dispatcher for EKS REST-JSON requests.
// It parses method+path, resolves to an AWS action, reads the body, invokes
// the per-action handler, and serialises the typed output as JSON. Errors
// are returned as plain awserrors codes; the caller (gateway.Request) routes
// them through the shared ErrorHandler, which now emits EKS REST-JSON.
func (gw *GatewayConfig) EKS_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupEKSAction(r.Method, r.URL.Path)
	if !ok {
		slog.Debug("EKS: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "eks", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("EKS_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("EKS_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Resolve the caller's IAM principal ARN best-effort; only CreateCluster
	// consumes it (bootstrap-creator-admin) and it degrades gracefully to "".
	callerARN := eksCallerPrincipalARN(r)

	output, err := handler(gw, accountID, callerARN, params, body)
	if err != nil {
		return err
	}

	gateway_eks.WriteJSONResponse(w, output)
	return nil
}

// eksCallerPrincipalARN resolves the caller's IAM principal ARN from the SigV4
// auth context, best-effort. Returns "" when the context is incomplete or the
// ARN can't be composed — CreateCluster then skips the creator-admin entry
// rather than failing the request.
func eksCallerPrincipalARN(r *http.Request) string {
	ctx := r.Context()
	accountID, _ := ctx.Value(ctxAccountID).(string)
	identity, _ := ctx.Value(ctxIdentity).(string)
	principalType, _ := ctx.Value(ctxPrincipalType).(string)
	assumedRoleARN, _ := ctx.Value(ctxAssumedRoleARN).(string)
	arn, err := buildCallerARN(accountID, identity, principalType, assumedRoleARN)
	if err != nil {
		slog.Debug("EKS_Request: could not resolve caller principal ARN", "err", err)
		return ""
	}
	return arn
}
