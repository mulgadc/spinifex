package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
	"log/slog"
	"net/http"
	"slices"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	gateway_eks "github.com/mulgadc/spinifex/spinifex/gateway/eks"
)

// eksRoute maps one HTTP method + chi path pattern to an AWS action and handler.
type eksRoute = rest.Route[eksRouteHandler]

// eksRouteHandler invokes a per-action EKS gateway function. callerARN is used
// by CreateCluster for the bootstrap-creator-admin AccessEntry; ignored by others.
type eksRouteHandler func(ctx context.Context, gw *GatewayConfig, accountID, callerARN string, params []string, body []byte) (any, error)

// eksRoutes is the dispatch table. Order is presentational: the router matches
// through a chi trie, which prefers a literal segment over a {param} one.
var eksRoutes = []eksRoute{
	// Cluster
	{Method: "POST", Pattern: "/clusters", Action: "CreateCluster",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateCluster(ctx, gw.NATSConn, acct, callerARN, b)
		}},
	{Method: "GET", Pattern: "/clusters", Action: "ListClusters",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListClusters(ctx, gw.NATSConn, acct)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/update-config", Action: "UpdateClusterConfig",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateClusterConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/updates", Action: "UpdateClusterVersion",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateClusterVersion(ctx, gw.NATSConn, acct, p[0], b)
		}},
	// The update surface these two read is absent, so they refuse here rather
	// than through a service method: a round-trip to reach a constant refusal
	// is surface for its own sake. Left unregistered they would answer
	// InvalidAction, which blames the caller's spelling for a routing gap.
	{Method: "GET", Pattern: "/clusters/{clusterName}/updates", Action: "ListUpdates",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return nil, errors.New(awserrors.ErrorNotImplemented)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/updates/{updateId}", Action: "DescribeUpdate",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return nil, errors.New(awserrors.ErrorNotImplemented)
		}},
	// Control-plane VM broker: relays bootstrap/state POSTs onto eks.bus.*/eks.state.* NATS subjects.
	// acct and callerARN are ignored; cluster account comes from the body.
	{Method: "POST", Pattern: "/clusters/{clusterName}/internal-publish", Action: "PublishInternal",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.PublishInternal(ctx, gw.NATSConn, p[0], b)
		}},
	// Token review broker: the eks-token-webhook POSTs bearer tokens here;
	// the gateway resolves them host-side (STS verify + AccessEntry lookup).
	{Method: "POST", Pattern: "/clusters/{clusterName}/token-review", Action: "WebhookTokenReview",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.WebhookTokenReview(ctx, gw.NATSConn, p[0], b)
		}},
	// Control-plane VM add-on delivery: the on-VM addon-sync agent GETs the set
	// of staged add-on manifests for its cluster (system SigV4 creds) to render
	// the baked bundles into the K3s auto-deploy dir. acct (system account) is
	// ignored — the cluster account is the {accountId} path segment, since a GET
	// carries no body to hold it (cf. PublishInternal). AuthorizeInternal binds
	// that segment to the caller's own cluster.
	{Method: "GET", Pattern: "/clusters/{clusterName}/internal-addons/{accountId}", Action: "ListInternalAddons",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListInternalAddons(ctx, gw.NATSConn, p[0], p[1])
		}},

	// Internal control-plane route: the on-VM k3s-recovery agent pulls its
	// per-member recovery directive (cluster-reset / wipe-rejoin) at boot. Same
	// system-cred carve-out as internal-addons; instance ID is the third segment.
	{Method: "GET", Pattern: "/clusters/{clusterName}/internal-recovery/{accountId}/{instanceId}", Action: "GetRecoveryDirective",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.GetRecoveryDirective(ctx, gw.NATSConn, p[0], p[1], p[2])
		}},

	// Nodegroup
	{Method: "POST", Pattern: "/clusters/{clusterName}/node-groups", Action: "CreateNodegroup",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateNodegroup(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/node-groups", Action: "ListNodegroups",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListNodegroups(ctx, gw.NATSConn, acct, p[0])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}/update-config", Action: "UpdateNodegroupConfig",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateNodegroupConfig(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}/update-version", Action: "UpdateNodegroupVersion",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateNodegroupVersion(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}", Action: "DescribeNodegroup",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeNodegroup(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}", Action: "DeleteNodegroup",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteNodegroup(ctx, gw.NATSConn, acct, p[0], p[1])
		}},

	// AccessEntry / AccessPolicy
	{Method: "POST", Pattern: "/clusters/{clusterName}/access-entries", Action: "CreateAccessEntry",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateAccessEntry(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/access-entries", Action: "ListAccessEntries",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAccessEntries(ctx, gw.NATSConn, acct, p[0])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}/access-policies", Action: "AssociateAccessPolicy",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.AssociateAccessPolicy(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}/access-policies/{policyArn}", Action: "DisassociateAccessPolicy",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DisassociateAccessPolicy(ctx, gw.NATSConn, acct, p[0], p[1], p[2])
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}/access-policies", Action: "ListAssociatedAccessPolicies",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAssociatedAccessPolicies(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}", Action: "DescribeAccessEntry",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAccessEntry(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}", Action: "UpdateAccessEntry",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateAccessEntry(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}", Action: "DeleteAccessEntry",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteAccessEntry(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{Method: "GET", Pattern: "/access-policies", Action: "ListAccessPolicies",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAccessPolicies(ctx, gw.NATSConn, acct)
		}},

	// Addons
	{Method: "GET", Pattern: "/addons/supported-versions", Action: "DescribeAddonVersions",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAddonVersions(ctx, gw.NATSConn, acct)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/addons", Action: "ListAddons",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListAddons(ctx, gw.NATSConn, acct, p[0])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/addons", Action: "CreateAddon",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.CreateAddon(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/addons/{addonName}/update", Action: "UpdateAddon",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UpdateAddon(ctx, gw.NATSConn, acct, p[0], p[1], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/addons/{addonName}", Action: "DescribeAddon",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeAddon(ctx, gw.NATSConn, acct, p[0], p[1])
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/addons/{addonName}", Action: "DeleteAddon",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteAddon(ctx, gw.NATSConn, acct, p[0], p[1])
		}},

	// OIDC identity-provider configs
	{Method: "POST", Pattern: "/clusters/{clusterName}/identity-provider-configs/associate", Action: "AssociateIdentityProviderConfig",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.AssociateIdentityProviderConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/identity-provider-configs/describe", Action: "DescribeIdentityProviderConfig",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeIdentityProviderConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/identity-provider-configs/disassociate", Action: "DisassociateIdentityProviderConfig",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DisassociateIdentityProviderConfig(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/identity-provider-configs", Action: "ListIdentityProviderConfigs",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListIdentityProviderConfigs(ctx, gw.NATSConn, acct, p[0])
		}},

	// Cluster CRUD — listed after more-specific /clusters/{name}/... routes.
	{Method: "GET", Pattern: "/clusters/{clusterName}", Action: "DescribeCluster",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DescribeCluster(ctx, gw.NATSConn, acct, p[0])
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}", Action: "DeleteCluster",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.DeleteCluster(ctx, gw.NATSConn, acct, p[0])
		}},

	// Tags
	{Method: "POST", Pattern: "/tags/*", Action: "TagResource",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.TagResource(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "DELETE", Pattern: "/tags/*", Action: "UntagResource",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.UntagResource(ctx, gw.NATSConn, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/tags/*", Action: "ListTagsForResource",
		Handler: func(ctx context.Context, gw *GatewayConfig, acct, callerARN string, p []string, b []byte) (any, error) {
			return gateway_eks.ListTagsForResource(ctx, gw.NATSConn, acct, p[0])
		}},
}

// eksActionNames returns the distinct actions in eksRoutes in stable order.
// Deduplicated because an action may be reachable by more than one route.
func eksActionNames() []string {
	names := make([]string, 0, len(eksRoutes))
	for _, route := range eksRoutes {
		names = append(names, route.Action)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// eksRouter matches an escaped request path against eksRoutes.
var eksRouter = rest.NewRouter("eks", eksRoutes)

// EKS_Request dispatches EKS REST-JSON requests: resolves method+path to an
// action, reads the body, calls the handler, and serialises the output as JSON.
func (gw *GatewayConfig) EKS_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := eksRouter.Lookup(r.Method, r.URL.EscapedPath())
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
	if err := gw.checkEKSPassRole(r, action, accountID, body); err != nil {
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

// errForeignRole marks a role ARN naming an account other than the caller's.
var errForeignRole = errors.New("role is not in the caller's account")

// checkEKSPassRole enforces iam:PassRole on each role action hands to EKS. Only
// the exact ARN the store holds resolves, so the grant is evaluated against the
// role's real path and an invented one cannot match a narrower Resource.
func (gw *GatewayConfig) checkEKSPassRole(r *http.Request, action, accountID string, body []byte) error {
	for _, roleARN := range gateway_eks.PassedRoleARNs(action, body) {
		_, _, resolveErr := auth.ResolveRoleARN(roleARN, func(roleAccount, roleName string) (string, error) {
			if roleAccount != accountID {
				return "", errForeignRole
			}
			return gw.IAMService.CanonicalResourceARN(roleAccount, arn.IAMRole, roleName)
		})
		if errors.Is(resolveErr, auth.ErrInvalidRoleARN) || errors.Is(resolveErr, errForeignRole) {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is not a role ARN in account %s", roleARN, accountID)
		}
		unknown := errors.Is(resolveErr, auth.ErrRoleARNMismatch) || awserrors.IsErrorCode(resolveErr, awserrors.ErrorIAMNoSuchEntity)
		if resolveErr != nil && !unknown {
			return resolveErr
		}
		// Evaluated before an unknown role is reported, so a caller without
		// iam:PassRole cannot use the reply to probe which roles exist.
		if err := gw.checkPassRole(r, roleARN, eksServicePrincipal); err != nil {
			return err
		}
		if unknown {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "role %s does not exist", roleARN)
		}
	}
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
