package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
)

// GenerateEKSErrorResponse returns a JSON {"__type":"<code>Exception","message":"<msg>"} body
// for use by writeClusterUnavailable, writeThrottleError, and ErrorHandler.
func GenerateEKSErrorResponse(code, message, _ string) []byte {
	return gateway_eks.GenerateEKSErrorResponse(code, message)
}

// jsonErrorType derives the X-Amzn-Errortype header value for code, mirroring
// GenerateEKSErrorResponse's own "Exception" suffixing so the header and the
// body's __type always agree.
func jsonErrorType(code string) string {
	if strings.HasSuffix(code, "Exception") {
		return code
	}
	return code + "Exception"
}

// eksRoute maps one HTTP method + chi path pattern to an AWS action and handler.
type eksRoute = restRoute[eksRouteHandler]

// eksRouteHandler invokes a per-action EKS gateway function. callerARN is used
// by CreateCluster for the bootstrap-creator-admin AccessEntry; ignored by others.
type eksRouteHandler func(ctx context.Context, gw *GatewayConfig, accountID, callerARN string, params []string, body []byte) (any, error)

// eksRoutes is the dispatch table. Order is presentational: the router matches
// through a chi trie, which prefers a literal segment over a {param} one.
var eksRoutes = []eksRoute{
	// Cluster
	{"POST", "/clusters", "CreateCluster",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateCluster(ctx, gw.NATSConn, acct, callerARN, b)
		}},
	{"GET", "/clusters", "ListClusters",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListClusters(ctx, gw.NATSConn, acct)
		}},
	{"POST", "/clusters/{clusterName}/update-config", "UpdateClusterConfig",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateClusterConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"POST", "/clusters/{clusterName}/updates", "UpdateClusterVersion",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateClusterVersion(ctx, gw.NATSConn, acct, p[0], b)
		}},
	// The update surface these two read is absent, so they refuse here rather
	// than through a service method: a round-trip to reach a constant refusal
	// is surface for its own sake. Left unregistered they would answer
	// InvalidAction, which blames the caller's spelling for a routing gap.
	{"GET", "/clusters/{clusterName}/updates", "ListUpdates",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return nil, errors.New(awserrors.ErrorNotImplemented)
		}},
	{"GET", "/clusters/{clusterName}/updates/{updateId}", "DescribeUpdate",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return nil, errors.New(awserrors.ErrorNotImplemented)
		}},
	// Control-plane VM broker: relays bootstrap/state POSTs onto eks.bus.*/eks.state.* NATS subjects.
	// acct and callerARN are ignored; cluster account comes from the body.
	{"POST", "/clusters/{clusterName}/internal-publish", "PublishInternal",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.PublishInternal(ctx, gw.NATSConn, p[0], b)
		}},
	// Token review broker: the eks-token-webhook POSTs bearer tokens here;
	// the gateway resolves them host-side (STS verify + AccessEntry lookup).
	{"POST", "/clusters/{clusterName}/token-review", "WebhookTokenReview",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.WebhookTokenReview(ctx, gw.NATSConn, p[0], b)
		}},
	// Control-plane VM add-on delivery: the on-VM addon-sync agent GETs the set
	// of staged add-on manifests for its cluster (system SigV4 creds) to render
	// the baked bundles into the K3s auto-deploy dir. acct (system account) is
	// ignored — the cluster account is the {accountId} path segment, since a GET
	// carries no body to hold it (cf. PublishInternal). AuthorizeInternal binds
	// that segment to the caller's own cluster.
	{"GET", "/clusters/{clusterName}/internal-addons/{accountId}", "ListInternalAddons",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListInternalAddons(ctx, gw.NATSConn, p[0], p[1])
		}},

	// Internal control-plane route: the on-VM k3s-recovery agent pulls its
	// per-member recovery directive (cluster-reset / wipe-rejoin) at boot. Same
	// system-cred carve-out as internal-addons; instance ID is the third segment.
	{"GET", "/clusters/{clusterName}/internal-recovery/{accountId}/{instanceId}", "GetRecoveryDirective",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.GetRecoveryDirective(ctx, gw.NATSConn, p[0], p[1], p[2])
		}},

	// Nodegroup
	{"POST", "/clusters/{clusterName}/node-groups", "CreateNodegroup",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateNodegroup(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"GET", "/clusters/{clusterName}/node-groups", "ListNodegroups",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListNodegroups(ctx, gw.NATSConn, acct, p[0])
		}},
	{"POST", "/clusters/{clusterName}/node-groups/{nodegroupName}/update-config", "UpdateNodegroupConfig",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateNodegroupConfig(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"POST", "/clusters/{clusterName}/node-groups/{nodegroupName}/update-version", "UpdateNodegroupVersion",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateNodegroupVersion(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"GET", "/clusters/{clusterName}/node-groups/{nodegroupName}", "DescribeNodegroup",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeNodegroup(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{"DELETE", "/clusters/{clusterName}/node-groups/{nodegroupName}", "DeleteNodegroup",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteNodegroup(ctx, gw.NATSConn, acct, p[0], p[1])
		}},

	// AccessEntry / AccessPolicy
	{"POST", "/clusters/{clusterName}/access-entries", "CreateAccessEntry",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateAccessEntry(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"GET", "/clusters/{clusterName}/access-entries", "ListAccessEntries",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAccessEntries(ctx, gw.NATSConn, acct, p[0])
		}},
	{"POST", "/clusters/{clusterName}/access-entries/{principalArn}/access-policies", "AssociateAccessPolicy",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.AssociateAccessPolicy(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"DELETE", "/clusters/{clusterName}/access-entries/{principalArn}/access-policies/{policyArn}", "DisassociateAccessPolicy",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DisassociateAccessPolicy(ctx, gw.NATSConn, acct, p[0], p[1], p[2])
		}},
	{"GET", "/clusters/{clusterName}/access-entries/{principalArn}/access-policies", "ListAssociatedAccessPolicies",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAssociatedAccessPolicies(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{"GET", "/clusters/{clusterName}/access-entries/{principalArn}", "DescribeAccessEntry",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAccessEntry(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{"POST", "/clusters/{clusterName}/access-entries/{principalArn}", "UpdateAccessEntry",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateAccessEntry(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"DELETE", "/clusters/{clusterName}/access-entries/{principalArn}", "DeleteAccessEntry",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteAccessEntry(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{"GET", "/access-policies", "ListAccessPolicies",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAccessPolicies(ctx, gw.NATSConn, acct)
		}},

	// Addons
	{"GET", "/addons/supported-versions", "DescribeAddonVersions",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAddonVersions(ctx, gw.NATSConn, acct)
		}},
	{"GET", "/clusters/{clusterName}/addons", "ListAddons",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAddons(ctx, gw.NATSConn, acct, p[0])
		}},
	{"POST", "/clusters/{clusterName}/addons", "CreateAddon",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateAddon(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"POST", "/clusters/{clusterName}/addons/{addonName}/update", "UpdateAddon",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateAddon(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{"GET", "/clusters/{clusterName}/addons/{addonName}", "DescribeAddon",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAddon(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{"DELETE", "/clusters/{clusterName}/addons/{addonName}", "DeleteAddon",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteAddon(ctx, gw.NATSConn, acct, p[0], p[1])
		}},

	// OIDC identity-provider configs
	{"POST", "/clusters/{clusterName}/identity-provider-configs/associate", "AssociateIdentityProviderConfig",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.AssociateIdentityProviderConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"POST", "/clusters/{clusterName}/identity-provider-configs/describe", "DescribeIdentityProviderConfig",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeIdentityProviderConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"POST", "/clusters/{clusterName}/identity-provider-configs/disassociate", "DisassociateIdentityProviderConfig",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DisassociateIdentityProviderConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"GET", "/clusters/{clusterName}/identity-provider-configs", "ListIdentityProviderConfigs",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListIdentityProviderConfigs(ctx, gw.NATSConn, acct, p[0])
		}},

	// Cluster CRUD — listed after more-specific /clusters/{name}/... routes.
	{"GET", "/clusters/{clusterName}", "DescribeCluster",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeCluster(ctx, gw.NATSConn, acct, p[0])
		}},
	{"DELETE", "/clusters/{clusterName}", "DeleteCluster",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteCluster(ctx, gw.NATSConn, acct, p[0])
		}},

	// Tags
	{"POST", "/tags/*", "TagResource",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.TagResource(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"DELETE", "/tags/*", "UntagResource",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UntagResource(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{"GET", "/tags/*", "ListTagsForResource",
		func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListTagsForResource(ctx, gw.NATSConn, acct, p[0])
		}},
}

// eksActionNames returns the distinct actions in eksRoutes in stable order.
// Deduplicated because an action may be reachable by more than one route.
func eksActionNames() []string {
	names := make([]string, 0, len(eksRoutes))
	for _, route := range eksRoutes {
		names = append(names, route.action)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// eksRouter matches an escaped request path against eksRoutes.
var eksRouter = newRESTRouter("eks", eksRoutes)

// EKS_Request dispatches EKS REST-JSON requests: resolves method+path to an
// action, reads the body, calls the handler, and serialises the output as JSON.
func (gw *GatewayConfig) EKS_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := eksRouter.lookup(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "EKS: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from it.
	// gw.Region, never the caller-supplied credential-scope region, which would
	// let a caller sign for another region and slide out from under a Deny.
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "EKS_Request: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	// Ahead of the policy check: the internal routes name the target account in
	// the path, so an eks:* grant evaluates as permitted and only the principal
	// class plus the caller's own instance say whether that account is its own.
	if gateway_eks.IsInternalAction(action) {
		if err := gateway_eks.AuthorizeInternal(r.Context(), gw.NATSConn, action, eksCaller(r), params); err != nil {
			return err
		}
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "EKS_Request: failed to read body", "err", err)
		return err
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

	// After the body read: CreateCluster and the other create actions name their
	// resource there rather than in the path.
	resources, err := gateway_eks.ResourceARNs(action, gw.Region, accountID, params, body)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "eks", action, resources); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Best-effort caller ARN; only CreateCluster consumes it, degrades to "".
	callerARN := eksCallerPrincipalARN(r)

	output, err := handler(r.Context(), gw, accountID, callerARN, params, body)
	if err != nil {
		return err
	}

	gateway_eks.WriteJSONResponse(w, output)
	return nil
}

// eksCaller reads the principal behind the request. The role name comes from the
// underlying role ARN, never the session name, which the caller picks at
// AssumeRole time and could name its way past the gate.
func eksCaller(r *http.Request) gateway_eks.Caller {
	accountID := mustCtxString(r, ctxAccountID)
	caller := gateway_eks.Caller{
		AccountID:     accountID,
		PrincipalType: mustCtxString(r, ctxPrincipalType),
		SessionName:   mustCtxString(r, ctxIdentity),
	}
	if roleARN := mustCtxString(r, ctxUnderlyingRoleARN); roleARN != "" {
		if roleAcct, roleName, err := auth.ParseRoleARN(roleARN); err == nil && roleAcct == accountID {
			caller.RoleName = roleName
		}
	}
	return caller
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
		slog.DebugContext(r.Context(), "EKS_Request: could not resolve caller principal ARN", "err", err)
		return ""
	}
	return arn
}
