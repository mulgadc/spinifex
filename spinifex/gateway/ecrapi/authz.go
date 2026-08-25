package gateway_ecrapi

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
)

// The resource a policy check evaluates against when the request names nothing
// in particular, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// repositoryResourceType is the ARN resource-type segment for a repository.
const repositoryResourceType = "repository"

// Where an action's resource is named in the JSON body.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
	sourceRepository
	sourceRepositories
	sourceTagARN
)

// Every action ECR_Request serves, with the resources AWS evaluates it against.
// Exhaustive by contract: a completeness test compares this table with the
// dispatch table in both directions, so an action cannot be added with a silent
// account-wide grant.
var ecrScopes = map[string][]resourceSource{
	// The registry-level surface AWS evaluates against "*": an authorization
	// token, the registry policy, replication and the scanning configuration
	// are properties of the registry, not of any one repository.
	"GetAuthorizationToken":                   {sourceAny},
	"GetRegistryPolicy":                       {sourceAny},
	"PutRegistryPolicy":                       {sourceAny},
	"DescribeRegistry":                        {sourceAny},
	"GetRegistryScanningConfiguration":        {sourceAny},
	"PutRegistryScanningConfiguration":        {sourceAny},
	"BatchGetRepositoryScanningConfiguration": {sourceAny},
	"PutReplicationConfiguration":             {sourceAny},
	// The repository list is account-level.
	"ListRepositories": {sourceAny},

	// Repositories.
	"CreateRepository":      {sourceRepository},
	"DeleteRepository":      {sourceRepository},
	"DescribeRepositories":  {sourceRepositories},
	"PutImageTagMutability": {sourceRepository},

	// Images and layers.
	"BatchGetImage":               {sourceRepository},
	"BatchCheckLayerAvailability": {sourceRepository},
	"BatchDeleteImage":            {sourceRepository},
	"PutImage":                    {sourceRepository},
	"ListImages":                  {sourceRepository},
	"DescribeImages":              {sourceRepository},
	"GetDownloadUrlForLayer":      {sourceRepository},
	"InitiateLayerUpload":         {sourceRepository},
	"UploadLayerPart":             {sourceRepository},
	"CompleteLayerUpload":         {sourceRepository},
	"ReplicateImage":              {sourceRepository},

	// Repository policy.
	"SetRepositoryPolicy":    {sourceRepository},
	"GetRepositoryPolicy":    {sourceRepository},
	"DeleteRepositoryPolicy": {sourceRepository},

	// Repository scanning.
	"PutImageScanningConfiguration": {sourceRepository},
	"GetImageScanningConfiguration": {sourceRepository},
	"StartImageScan":                {sourceRepository},
	"DescribeImageScanFindings":     {sourceRepository},

	// Lifecycle policy.
	"PutLifecyclePolicy":          {sourceRepository},
	"GetLifecyclePolicy":          {sourceRepository},
	"DeleteLifecyclePolicy":       {sourceRepository},
	"StartLifecyclePolicyPreview": {sourceRepository},
	"GetLifecyclePolicyPreview":   {sourceRepository},

	// Tags.
	"TagResource":         {sourceTagARN},
	"UntagResource":       {sourceTagARN},
	"ListTagsForResource": {sourceTagARN},
}

// HasScope reports whether action has an explicit ECR scope-table entry.
func HasScope(action string) bool {
	_, ok := ecrScopes[action]
	return ok
}

// ScopedActions returns every action represented in the ECR scope table.
func ScopedActions() []string {
	actions := make([]string, 0, len(ecrScopes))
	for action := range ecrScopes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

// ResourceARNs resolves the resources an ECR control-plane request authorizes
// against from the same body bytes the handler will unmarshal. The registryId a
// body may carry is ignored: a caller-supplied account would let a request slide
// out from under a Deny scoped to the real one.
func ResourceARNs(action, region, accountID string, body []byte) ([]string, error) {
	sources, ok := ecrScopes[action]
	if !ok {
		slog.Error("ECR authz: action is served but absent from the scope table", "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	scope := bodyscope.Parse(action, body)
	resources := make([]string, 0, len(sources))
	for _, source := range sources {
		resolved, err := resolve(source, action, region, accountID, scope)
		if err != nil {
			return nil, err
		}
		for _, resource := range resolved {
			// An unresolved member drops out rather than contributing "*", which
			// no scoped Allow can match and which would deny a call AWS permits.
			if resource == anyResource || slices.Contains(resources, resource) {
				continue
			}
			resources = append(resources, resource)
		}
	}
	if len(resources) == 0 {
		return []string{anyResource}, nil
	}
	return resources, nil
}

func resolve(source resourceSource, action, region, accountID string, scope bodyscope.Scope) ([]string, error) {
	switch source {
	case sourceAny:
		return nil, nil

	case sourceRepository:
		return []string{repositoryARN(region, accountID, scope.String("repositoryName"))}, nil

	case sourceRepositories:
		names := scope.Strings("repositoryNames")
		out := make([]string, 0, len(names))
		for _, name := range names {
			out = append(out, repositoryARN(region, accountID, name))
		}
		return out, nil

	case sourceTagARN:
		return []string{tagARN(region, accountID, scope.String("resourceArn"))}, nil

	default:
		slog.Error("ECR authz: unhandled resource source, failing closed", "action", action, "source", source)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
}

// An absent identifier authorizes account-wide, so a malformed request stays
// the handler's validation fault rather than becoming an authorization failure.
func repositoryARN(region, accountID, name string) string {
	if region == "" || accountID == "" || name == "" {
		return anyResource
	}
	return fmt.Sprintf("arn:aws:ecr:%s:%s:%s/%s", region, accountID, repositoryResourceType, name)
}

// tagARN re-anchors the caller-supplied resource ARN on gw.Region and the
// caller's account: the tag handler keys off the repository name alone and
// operates in the caller's own account bucket.
func tagARN(region, accountID, resourceARN string) string {
	parts := strings.SplitN(resourceARN, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "ecr" {
		return anyResource
	}
	kind, name, found := strings.Cut(parts[5], "/")
	if !found || kind != repositoryResourceType || name == "" {
		return anyResource
	}
	return repositoryARN(region, accountID, name)
}
