package gateway_eks

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
)

// The resource a policy check evaluates against when the request names nothing
// in particular, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// Where an action's resource is named. Path parameters come from the route
// regex; body sources are read from the same JSON the handler unmarshals.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
	sourceCluster
	sourceClusterFromBody
	sourceInternalCluster
	sourceBodyAccountCluster
	sourceNodegroup
	sourceNodegroupFromBody
	sourceAddon
	sourceAddonFromBody
	sourceAccessEntry
	sourceAccessEntryFromBody
	sourceTagARN
)

// Every action EKS_Request serves, with the resources AWS evaluates it against.
// Exhaustive by contract: a completeness test compares this table with the
// dispatch table in both directions.
var eksScopes = map[string][]resourceSource{
	// Cluster.
	"CreateCluster":        {sourceClusterFromBody},
	"DescribeCluster":      {sourceCluster},
	"DeleteCluster":        {sourceCluster},
	"UpdateClusterConfig":  {sourceCluster},
	"UpdateClusterVersion": {sourceCluster},
	"ListClusters":         {sourceAny},

	// Internal control-plane routes. The cluster's owning account is a path
	// segment on these two, and the ARN names that account rather than the
	// system caller's, because that is the account the handler reads.
	"ListInternalAddons":   {sourceInternalCluster},
	"GetRecoveryDirective": {sourceInternalCluster},
	// The owning account arrives in the body on these two, and the ARN names
	// that account for the same reason: it is the account the handler acts in.
	"PublishInternal":    {sourceBodyAccountCluster},
	"WebhookTokenReview": {sourceBodyAccountCluster},

	// Nodegroups. Create evaluates the cluster and the nodegroup it is about
	// to create, matching AWS.
	"CreateNodegroup":        {sourceCluster, sourceNodegroupFromBody},
	"DescribeNodegroup":      {sourceNodegroup},
	"DeleteNodegroup":        {sourceNodegroup},
	"UpdateNodegroupConfig":  {sourceNodegroup},
	"UpdateNodegroupVersion": {sourceNodegroup},
	"ListNodegroups":         {sourceCluster},

	// Access entries and policies.
	"CreateAccessEntry":            {sourceCluster, sourceAccessEntryFromBody},
	"DescribeAccessEntry":          {sourceAccessEntry},
	"UpdateAccessEntry":            {sourceAccessEntry},
	"DeleteAccessEntry":            {sourceAccessEntry},
	"ListAccessEntries":            {sourceCluster},
	"AssociateAccessPolicy":        {sourceAccessEntry},
	"DisassociateAccessPolicy":     {sourceAccessEntry},
	"ListAssociatedAccessPolicies": {sourceAccessEntry},
	// The AWS-managed policy catalogue is not an account resource.
	"ListAccessPolicies": {sourceAny},

	// Add-ons.
	"CreateAddon":   {sourceCluster, sourceAddonFromBody},
	"DescribeAddon": {sourceAddon},
	"UpdateAddon":   {sourceAddon},
	"DeleteAddon":   {sourceAddon},
	"ListAddons":    {sourceCluster},
	// The add-on version catalogue is not an account resource.
	"DescribeAddonVersions": {sourceAny},

	// Identity-provider configs. All four handlers are stubs and no
	// identity-provider-config object exists here, so the cluster is the only
	// resource these name.
	"AssociateIdentityProviderConfig":    {sourceCluster},
	"DescribeIdentityProviderConfig":     {sourceCluster},
	"DisassociateIdentityProviderConfig": {sourceCluster},
	"ListIdentityProviderConfigs":        {sourceCluster},

	// Tags.
	"TagResource":         {sourceTagARN},
	"UntagResource":       {sourceTagARN},
	"ListTagsForResource": {sourceTagARN},
}

// HasScope reports whether action has an explicit EKS scope-table entry.
func HasScope(action string) bool {
	_, ok := eksScopes[action]
	return ok
}

// ScopedActions returns every action represented in the EKS scope table.
func ScopedActions() []string {
	return slices.Sorted(maps.Keys(eksScopes))
}

// ResourceARNs resolves the resources an EKS request authorizes against from
// the path parameters and body the dispatcher already holds. params are the
// route captures, already percent-decoded.
func ResourceARNs(action, region, accountID string, params []string, body []byte) ([]string, error) {
	sources, ok := eksScopes[action]
	if !ok {
		slog.Error("EKS authz: action is served but absent from the scope table", "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	resources := make([]string, 0, len(sources))
	for _, source := range sources {
		resource, err := resolve(source, action, region, accountID, params, body)
		if err != nil {
			return nil, err
		}
		// An unresolved member drops out rather than contributing "*", which no
		// scoped Allow can match and which would deny a call AWS permits.
		if resource == anyResource || slices.Contains(resources, resource) {
			continue
		}
		resources = append(resources, resource)
	}
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

func resolve(source resourceSource, action, region, accountID string, params []string, body []byte) (string, error) {
	switch source {
	case sourceAny:
		return anyResource, nil

	case sourceCluster:
		return clusterARN(region, accountID, param(params, 0)), nil

	case sourceClusterFromBody:
		input := new(eks.CreateClusterInput)
		if !unmarshalScope(action, body, input) {
			return anyResource, nil
		}
		return clusterARN(region, accountID, aws.StringValue(input.Name)), nil

	case sourceInternalCluster:
		return clusterARN(region, param(params, 1), param(params, 0)), nil

	case sourceBodyAccountCluster:
		scope, err := bodyscope.Parse(action, body)
		if err != nil {
			return "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		return clusterARN(region, scope.String("accountId"), param(params, 0)), nil

	case sourceNodegroup:
		return nodegroupARN(region, accountID, param(params, 0), param(params, 1)), nil

	case sourceNodegroupFromBody:
		input := new(eks.CreateNodegroupInput)
		if !unmarshalScope(action, body, input) {
			return anyResource, nil
		}
		return nodegroupARN(region, accountID, param(params, 0), aws.StringValue(input.NodegroupName)), nil

	case sourceAddon:
		return addonARN(region, accountID, param(params, 0), param(params, 1)), nil

	case sourceAddonFromBody:
		input := new(eks.CreateAddonInput)
		if !unmarshalScope(action, body, input) {
			return anyResource, nil
		}
		return addonARN(region, accountID, param(params, 0), aws.StringValue(input.AddonName)), nil

	case sourceAccessEntry:
		return accessEntryARN(region, accountID, param(params, 0), param(params, 1)), nil

	case sourceAccessEntryFromBody:
		input := new(eks.CreateAccessEntryInput)
		if !unmarshalScope(action, body, input) {
			return anyResource, nil
		}
		return accessEntryARN(region, accountID, param(params, 0), aws.StringValue(input.PrincipalArn)), nil

	case sourceTagARN:
		return tagARN(region, accountID, param(params, 0)), nil

	default:
		slog.Error("EKS authz: unhandled resource source, failing closed", "action", action, "source", source)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}

// An absent identifier authorizes account-wide, so a malformed request stays
// the handler's validation fault rather than becoming an authorization failure.
func clusterARN(region, accountID, cluster string) string {
	if region == "" || accountID == "" || cluster == "" {
		return anyResource
	}
	return arn.FormatEKSCluster(region, accountID, cluster)
}

func nodegroupARN(region, accountID, cluster, nodegroup string) string {
	if region == "" || accountID == "" || cluster == "" || nodegroup == "" {
		return anyResource
	}
	// The nodegroup ARN's trailing segment is derived from these same
	// identifiers at create time, so this is the ARN on the record, exactly.
	return arn.FormatEKSNodegroup(region, accountID, cluster, nodegroup,
		arn.EKSNodegroupDiscriminator(accountID, cluster, nodegroup))
}

func addonARN(region, accountID, cluster, addon string) string {
	if region == "" || accountID == "" || cluster == "" || addon == "" {
		return anyResource
	}
	return arn.FormatEKSAddon(region, accountID, cluster, addon)
}

func accessEntryARN(region, accountID, cluster, principalARN string) string {
	if region == "" || accountID == "" || cluster == "" || principalARN == "" {
		return anyResource
	}
	return arn.FormatEKSAccessEntry(region, accountID, cluster, handlers_eks.PrincipalARNHash(principalARN))
}

// tagARN re-anchors the caller-supplied resource ARN on gw.Region and the
// caller's account, because the tag handler ignores those segments and operates
// in the caller's own account bucket. A nodegroup's discriminator is re-derived
// for the same reason: the handler keys off (cluster, nodegroup) alone.
func tagARN(region, accountID, resourceARN string) string {
	kind, resource, ok := splitEKSARN(resourceARN)
	if !ok || region == "" || accountID == "" {
		return anyResource
	}
	switch kind {
	case arn.EKSCluster:
		return clusterARN(region, accountID, resource)
	case arn.EKSNodegroup:
		segments := strings.SplitN(resource, "/", 3)
		if len(segments) < 2 {
			return anyResource
		}
		return nodegroupARN(region, accountID, segments[0], segments[1])
	default:
		return arn.FormatEKSResource(region, accountID, string(kind)+"/"+resource)
	}
}

// splitEKSARN reports the resource type and the component after it, using the
// same shape rules the tag handler applies. An ARN it does not recognise
// resolves to "*" so the NotImplemented response stays the handler's.
func splitEKSARN(resourceARN string) (arn.EKSResourceType, string, bool) {
	parts := strings.SplitN(resourceARN, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "eks" {
		return "", "", false
	}
	rawKind, resource, found := strings.Cut(parts[5], "/")
	if !found || resource == "" {
		return "", "", false
	}
	kind := arn.EKSResourceType(rawKind)
	switch kind {
	case arn.EKSCluster, arn.EKSNodegroup, arn.EKSAddon, arn.EKSAccessEntry:
		return kind, resource, true
	default:
		return "", "", false
	}
}

// unmarshalScope reads the identifier-bearing fields of a request body. A body
// the gate cannot parse authorizes account-wide and leaves the rejection to the
// handler, which unmarshals the same bytes.
func unmarshalScope(action string, body []byte, input any) bool {
	if len(body) == 0 {
		return false
	}
	if err := json.Unmarshal(body, input); err != nil {
		slog.Debug("EKS authz: body does not parse, authorizing account-wide", "action", action, "err", err)
		return false
	}
	return true
}

func param(params []string, i int) string {
	if i >= len(params) {
		return ""
	}
	return params[i]
}
