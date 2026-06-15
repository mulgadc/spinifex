package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
)

// GenerateEKSErrorResponse returns a JSON {"__type":"<code>Exception","message":"<msg>"} body
// for use by writeClusterUnavailable, writeThrottleError, and ErrorHandler.
func GenerateEKSErrorResponse(code, message, _ string) []byte {
	return gateway_eks.GenerateEKSErrorResponse(code, message)
}

// eksRoute maps one HTTP method + path regex to an AWS action and handler.
type eksRoute struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler eksRouteHandler
}

// eksRouteHandler invokes a per-action EKS gateway function. callerARN is used
// by CreateCluster for the bootstrap-creator-admin AccessEntry; ignored by others.
type eksRouteHandler func(gw *GatewayConfig, accountID, callerARN string, params []string, body []byte) (any, error)

// eksRoutes is the dispatch table. More-specific paths must precede less-specific
// ones with the same prefix so the regex matcher picks the deeper route first.
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
	// Control-plane VM broker: relays bootstrap/state POSTs onto eks.bus.*/eks.state.* NATS subjects.
	// acct and callerARN are ignored; cluster account comes from the body.
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/internal-publish$`), "PublishInternal",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.PublishInternal(gw.NATSConn, p[0], b)
		}},
	// Token review broker: the eks-token-webhook POSTs bearer tokens here;
	// the gateway resolves them host-side (STS verify + AccessEntry lookup).
	{"POST", regexp.MustCompile(`^/clusters/([^/]+)/token-review$`), "WebhookTokenReview",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.WebhookTokenReview(gw.NATSConn, p[0], b)
		}},
	// Control-plane VM add-on delivery: the on-VM addon-sync agent GETs the set
	// of staged add-on manifests for its cluster (system SigV4 creds) to render
	// the baked bundles into the K3s auto-deploy dir. acct (system account) is
	// ignored — the cluster account is the {accountId} path segment, since a GET
	// carries no body to hold it (cf. PublishInternal).
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/internal-addons/([^/]+)$`), "ListInternalAddons",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListInternalAddons(gw.NATSConn, p[0], p[1])
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
	{"GET", regexp.MustCompile(`^/clusters/([^/]+)/addons$`), "ListAddons",
		func(gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAddons(gw.NATSConn, acct, p[0])
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

	// Cluster CRUD — listed after more-specific /clusters/{name}/... routes.
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

// lookupEKSAction matches method+path against eksRoutes, returning the action,
// path params, and handler, or ("", nil, nil, false) on no match.
// path must be r.URL.EscapedPath(): ARNs in path segments are percent-encoded
// by the CLI (e.g. user%2Fadmin), and matching the decoded path would break
// the [^/]+ capture. Captured params are PathUnescape'd before returning.
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
			params = make([]string, 0, len(m)-1)
			for _, raw := range m[1:] {
				decoded, err := url.PathUnescape(raw)
				if err != nil {
					slog.Debug("EKS: bad percent-encoding in path param", "param", raw, "err", err)
					decoded = raw
				}
				params = append(params, decoded)
			}
		}
		return route.action, params, route.handler, true
	}
	return "", nil, nil, false
}

// EKS_Request dispatches EKS REST-JSON requests: resolves method+path to an
// action, reads the body, calls the handler, and serialises the output as JSON.
func (gw *GatewayConfig) EKS_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupEKSAction(r.Method, r.URL.EscapedPath())
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

	// Some REST-JSON actions carry their non-path inputs as query params with
	// an empty body (e.g. UntagResource's tagKeys arrive as
	// DELETE /tags/{arn}?tagKeys=k1&tagKeys=k2). url.Values is map[string][]string,
	// so it marshals straight to the JSON the per-action unmarshal expects
	// ({"tagKeys":["k1","k2"]}). Only folds when the body is empty so it never
	// shadows a real payload.
	if len(body) == 0 {
		if q := r.URL.Query(); len(q) > 0 {
			if qb, err := json.Marshal(map[string][]string(q)); err == nil {
				body = qb
			}
		}
	}

	// Best-effort caller ARN; only CreateCluster consumes it, degrades to "".
	callerARN := eksCallerPrincipalARN(r)

	output, err := handler(gw, accountID, callerARN, params, body)
	if err != nil {
		return err
	}

	gateway_eks.WriteJSONResponse(w, output)
	return nil
}

// eksCallerPrincipalARN resolves the caller's IAM principal ARN from the SigV4
// auth context. Returns "" when the ARN can't be composed; CreateCluster then
// skips the creator-admin AccessEntry rather than failing.
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
