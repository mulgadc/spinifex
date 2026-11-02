package awsapi

import (
	"context"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/rest"
	"github.com/nats-io/nats.go"
)

// route maps one HTTP method + chi path pattern to an AWS action and handler.
type route = rest.Route[routeHandler]

// routeHandler invokes a per-action EKS gateway function. callerARN is used
// by CreateCluster for the bootstrap-creator-admin AccessEntry; ignored by others.
type routeHandler func(ctx context.Context, nc *nats.Conn, accountID, callerARN string, params []string, body []byte) (any, error)

// routes is the dispatch table. Order is presentational: the router matches
// through a chi trie, which prefers a literal segment over a {param} one.
var routes = []route{
	// Cluster
	{Method: "POST", Pattern: "/clusters", Action: "CreateCluster",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return CreateCluster(ctx, nc, acct, callerARN, b)
		}},
	{Method: "GET", Pattern: "/clusters", Action: "ListClusters",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListClusters(ctx, nc, acct)
		}},
	// Control-plane VM broker: relays bootstrap/state/addon POSTs onto eks.bus/state/addon subjects.
	// acct and callerARN are ignored; the cluster account comes from the body, and
	// AuthorizeInternal binds that account and cluster to the caller's own.
	{Method: "POST", Pattern: "/clusters/{clusterName}/internal-publish", Action: "PublishInternal",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return PublishInternal(ctx, nc, p[0], b)
		}},
	// Token review broker: the eks-token-webhook POSTs bearer tokens here and the
	// gateway resolves them host-side. The account comes from the body, and
	// AuthorizeInternal binds that account and cluster to the caller's own.
	{Method: "POST", Pattern: "/clusters/{clusterName}/token-review", Action: "WebhookTokenReview",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return WebhookTokenReview(ctx, nc, p[0], b)
		}},
	// Control-plane VM add-on delivery: the on-VM addon-sync agent GETs the set
	// of staged add-on manifests for its cluster (system SigV4 creds) to render
	// the baked bundles into the K3s auto-deploy dir. acct (system account) is
	// ignored — the cluster account is the {accountId} path segment, since a GET
	// carries no body to hold it (cf. PublishInternal). AuthorizeInternal binds
	// that segment to the caller's own cluster.
	{Method: "GET", Pattern: "/clusters/{clusterName}/internal-addons/{accountId}", Action: "ListInternalAddons",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListInternalAddons(ctx, nc, p[0], p[1])
		}},

	// Internal control-plane route: the on-VM k3s-recovery agent pulls its
	// per-member recovery directive (cluster-reset / wipe-rejoin) at boot. Same
	// system-cred carve-out as internal-addons; instance ID is the third segment.
	{Method: "GET", Pattern: "/clusters/{clusterName}/internal-recovery/{accountId}/{instanceId}", Action: "GetRecoveryDirective",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return GetRecoveryDirective(ctx, nc, p[0], p[1], p[2])
		}},

	// Internal control-plane route: the on-VM node-gc agent pulls the cluster's
	// live nodegroup workers to delete the Nodes of terminated ones. Same
	// system-cred carve-out as internal-addons.
	{Method: "GET", Pattern: "/clusters/{clusterName}/internal-workers/{accountId}", Action: "ListInternalWorkers",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListInternalWorkers(ctx, nc, p[0], p[1])
		}},

	// Nodegroup
	{Method: "POST", Pattern: "/clusters/{clusterName}/node-groups", Action: "CreateNodegroup",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return CreateNodegroup(ctx, nc, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/node-groups", Action: "ListNodegroups",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListNodegroups(ctx, nc, acct, p[0])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}/update-config", Action: "UpdateNodegroupConfig",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return UpdateNodegroupConfig(ctx, nc, acct, p[0], p[1], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}", Action: "DescribeNodegroup",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DescribeNodegroup(ctx, nc, acct, p[0], p[1])
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/node-groups/{nodegroupName}", Action: "DeleteNodegroup",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DeleteNodegroup(ctx, nc, acct, p[0], p[1])
		}},

	// AccessEntry / AccessPolicy
	{Method: "POST", Pattern: "/clusters/{clusterName}/access-entries", Action: "CreateAccessEntry",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return CreateAccessEntry(ctx, nc, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/access-entries", Action: "ListAccessEntries",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListAccessEntries(ctx, nc, acct, p[0])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}/access-policies", Action: "AssociateAccessPolicy",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return AssociateAccessPolicy(ctx, nc, acct, p[0], p[1], b)
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}/access-policies/{policyArn}", Action: "DisassociateAccessPolicy",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DisassociateAccessPolicy(ctx, nc, acct, p[0], p[1], p[2])
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}/access-policies", Action: "ListAssociatedAccessPolicies",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListAssociatedAccessPolicies(ctx, nc, acct, p[0], p[1])
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}", Action: "DescribeAccessEntry",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DescribeAccessEntry(ctx, nc, acct, p[0], p[1])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}", Action: "UpdateAccessEntry",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return UpdateAccessEntry(ctx, nc, acct, p[0], p[1], b)
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/access-entries/{principalArn}", Action: "DeleteAccessEntry",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DeleteAccessEntry(ctx, nc, acct, p[0], p[1])
		}},
	{Method: "GET", Pattern: "/access-policies", Action: "ListAccessPolicies",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListAccessPolicies(ctx, nc, acct)
		}},

	// Addons
	{Method: "GET", Pattern: "/addons/supported-versions", Action: "DescribeAddonVersions",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DescribeAddonVersions(ctx, nc, acct, b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/addons", Action: "ListAddons",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListAddons(ctx, nc, acct, p[0])
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/addons", Action: "CreateAddon",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return CreateAddon(ctx, nc, acct, p[0], b)
		}},
	{Method: "POST", Pattern: "/clusters/{clusterName}/addons/{addonName}/update", Action: "UpdateAddon",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return UpdateAddon(ctx, nc, acct, p[0], p[1], b)
		}},
	{Method: "GET", Pattern: "/clusters/{clusterName}/addons/{addonName}", Action: "DescribeAddon",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DescribeAddon(ctx, nc, acct, p[0], p[1])
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}/addons/{addonName}", Action: "DeleteAddon",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DeleteAddon(ctx, nc, acct, p[0], p[1])
		}},

	// Cluster CRUD — listed after more-specific /clusters/{name}/... routes.
	{Method: "GET", Pattern: "/clusters/{clusterName}", Action: "DescribeCluster",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DescribeCluster(ctx, nc, acct, p[0])
		}},
	{Method: "DELETE", Pattern: "/clusters/{clusterName}", Action: "DeleteCluster",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return DeleteCluster(ctx, nc, acct, p[0])
		}},

	// Tags
	{Method: "POST", Pattern: "/tags/*", Action: "TagResource",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return TagResource(ctx, nc, acct, p[0], b)
		}},
	{Method: "DELETE", Pattern: "/tags/*", Action: "UntagResource",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return UntagResource(ctx, nc, acct, p[0], b)
		}},
	{Method: "GET", Pattern: "/tags/*", Action: "ListTagsForResource",
		Handler: func(ctx context.Context, nc *nats.Conn, acct, callerARN string, p []string, b []byte) (any, error) {
			return ListTagsForResource(ctx, nc, acct, p[0])
		}},
}

// actionNames returns the distinct actions in routes in stable order.
// Deduplicated because an action may be reachable by more than one route.
func actionNames() []string {
	names := make([]string, 0, len(routes))
	for _, route := range routes {
		names = append(names, route.Action)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// router matches an escaped request path against routes.
var router = rest.NewRouter("eks", routes)
