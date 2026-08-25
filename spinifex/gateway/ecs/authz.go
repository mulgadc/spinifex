package gateway_ecs

import (
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/bodyscope"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
)

// The resource a policy check evaluates against when the request names nothing
// in particular, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// A task-definition reference without a revision means "latest", which the gate
// cannot resolve without a store read. The literal "*" is a value, so it matches
// the AWS-documented spelling without widening a grant.
const anyRevision = "*"

// Where an action's resource is named in the JSON body. Cluster-scoped sources
// read the request's "cluster" field alongside their own identifier.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
	sourceCluster
	sourceNewCluster
	sourceClusters
	sourceService
	sourceNewService
	sourceServices
	sourceTask
	sourceTasks
	sourceContainerInstance
	sourceContainerInstances
	sourceTaskDefinition
	sourceNewCapacityProvider
	sourceCapacityProvider
	sourceCapacityProviders
	sourceTagARN
)

// Every action ECS_Request serves, with the resources AWS evaluates it against.
// Exhaustive by contract: a completeness test compares this table with the
// dispatch table in both directions, so an action cannot be added with a silent
// account-wide grant.
var ecsScopes = map[string][]resourceSource{
	// Clusters.
	"CreateCluster":               {sourceNewCluster},
	"DeleteCluster":               {sourceCluster},
	"UpdateCluster":               {sourceCluster},
	"DescribeClusters":            {sourceClusters},
	"PutClusterCapacityProviders": {sourceCluster},
	// Internal capacity launch. Not an AWS action, but it names the cluster it
	// launches container instances into.
	"ProvisionCapacity": {sourceCluster},
	// The cluster list is account-level.
	"ListClusters": {sourceAny},

	// Services.
	"CreateService":    {sourceNewService},
	"UpdateService":    {sourceService},
	"DeleteService":    {sourceService},
	"DescribeServices": {sourceServices},
	// AWS documents no resource type for the service lists.
	"ListServices":            {sourceAny},
	"ListServicesByNamespace": {sourceAny},

	// Tasks.
	"StopTask":      {sourceTask},
	"DescribeTasks": {sourceTasks},
	// Internal agent report; it names the task it reports devices for.
	"ReportTaskGPU": {sourceTask},
	// AWS documents no resource type for ListTasks, and a state change names
	// its task by ARN in a field the handler resolves rather than the gate.
	"ListTasks":             {sourceAny},
	"SubmitTaskStateChange": {sourceAny},

	// Container instances.
	"DeregisterContainerInstance":   {sourceContainerInstance},
	"DescribeContainerInstances":    {sourceContainerInstances},
	"UpdateContainerInstancesState": {sourceContainerInstances},
	// Internal agent poll; it names the instance whose inbox it drains.
	"PollAssignments": {sourceContainerInstance},
	// A container instance has no id until the handler mints one, and AWS
	// documents no resource type for the list.
	"RegisterContainerInstance": {sourceAny},
	"ListContainerInstances":    {sourceAny},

	// Task definitions. AWS evaluates a run against the definition it launches.
	"DeregisterTaskDefinition": {sourceTaskDefinition},
	"RunTask":                  {sourceTaskDefinition},
	"StartTask":                {sourceTaskDefinition},
	// AWS documents no resource type for these four.
	"RegisterTaskDefinition":     {sourceAny},
	"DescribeTaskDefinition":     {sourceAny},
	"ListTaskDefinitions":        {sourceAny},
	"ListTaskDefinitionFamilies": {sourceAny},

	// Capacity providers.
	"CreateCapacityProvider":    {sourceNewCapacityProvider},
	"DeleteCapacityProvider":    {sourceCapacityProvider},
	"DescribeCapacityProviders": {sourceCapacityProviders},

	// Account settings are account-level.
	"PutAccountSetting":   {sourceAny},
	"ListAccountSettings": {sourceAny},

	// Tags.
	"TagResource":         {sourceTagARN},
	"UntagResource":       {sourceTagARN},
	"ListTagsForResource": {sourceTagARN},
}

// HasScope reports whether action has an explicit ECS scope-table entry.
func HasScope(action string) bool {
	_, ok := ecsScopes[action]
	return ok
}

// ScopedActions returns every action represented in the ECS scope table.
func ScopedActions() []string {
	actions := make([]string, 0, len(ecsScopes))
	for action := range ecsScopes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

// ResourceARNs resolves the resources an ECS request authorizes against from
// the same body bytes the handler will unmarshal.
func ResourceARNs(action, region, accountID string, body []byte) ([]string, error) {
	sources, ok := ecsScopes[action]
	if !ok {
		slog.Error("ECS authz: action is served but absent from the scope table", "action", action)
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
	// The handler resolves an omitted cluster to "default", so the gate must
	// too, or a fence on the default cluster never fires.
	cluster := handlers_ecs.ClusterShortName(scope.String("cluster"))

	switch source {
	case sourceAny:
		return nil, nil

	case sourceCluster:
		return one(clusterARN(region, accountID, cluster)), nil

	case sourceNewCluster:
		name := handlers_ecs.ClusterShortName(scope.String("clusterName"))
		return one(clusterARN(region, accountID, name)), nil

	case sourceClusters:
		return each(scope.Strings("clusters"), func(ref string) string {
			return clusterARN(region, accountID, handlers_ecs.ClusterShortName(ref))
		}), nil

	case sourceService:
		return one(serviceARN(region, accountID, cluster, scope.String("service"))), nil

	case sourceNewService:
		return one(serviceARN(region, accountID, cluster, scope.String("serviceName"))), nil

	case sourceServices:
		return each(scope.Strings("services"), func(ref string) string {
			return serviceARN(region, accountID, cluster, ref)
		}), nil

	case sourceTask:
		return one(taskARN(region, accountID, cluster, scope.String("task"))), nil

	case sourceTasks:
		return each(scope.Strings("tasks"), func(ref string) string {
			return taskARN(region, accountID, cluster, ref)
		}), nil

	case sourceContainerInstance:
		return one(containerInstanceARN(region, accountID, cluster, scope.String("containerInstance"))), nil

	case sourceContainerInstances:
		return each(scope.Strings("containerInstances"), func(ref string) string {
			return containerInstanceARN(region, accountID, cluster, ref)
		}), nil

	case sourceTaskDefinition:
		return one(taskDefARN(region, accountID, scope.String("taskDefinition"))), nil

	case sourceNewCapacityProvider:
		return one(capacityProviderARN(region, accountID, scope.String("name"))), nil

	case sourceCapacityProvider:
		return one(capacityProviderARN(region, accountID, scope.String("capacityProvider"))), nil

	case sourceCapacityProviders:
		return each(scope.Strings("capacityProviders"), func(ref string) string {
			return capacityProviderARN(region, accountID, ref)
		}), nil

	case sourceTagARN:
		return one(tagARN(region, accountID, scope.String("resourceArn"))), nil

	default:
		slog.Error("ECS authz: unhandled resource source, failing closed", "action", action, "source", source)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
}

func one(resource string) []string {
	return []string{resource}
}

func each(refs []string, build func(string) string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, build(ref))
	}
	return out
}

// An absent identifier authorizes account-wide, so a malformed request stays
// the handler's validation fault rather than becoming an authorization failure.
func clusterARN(region, accountID, cluster string) string {
	if region == "" || accountID == "" || cluster == "" {
		return anyResource
	}
	return handlers_ecs.ClusterARN(region, accountID, cluster)
}

func serviceARN(region, accountID, cluster, ref string) string {
	name := handlers_ecs.ServiceShortName(ref)
	if region == "" || accountID == "" || cluster == "" || name == "" {
		return anyResource
	}
	return handlers_ecs.ServiceARN(region, accountID, cluster, name)
}

func taskARN(region, accountID, cluster, ref string) string {
	id := handlers_ecs.TaskShortID(ref)
	if region == "" || accountID == "" || cluster == "" || id == "" {
		return anyResource
	}
	return handlers_ecs.TaskARN(region, accountID, cluster, id)
}

func containerInstanceARN(region, accountID, cluster, ref string) string {
	id := handlers_ecs.ContainerInstanceShortID(ref)
	if region == "" || accountID == "" || cluster == "" || id == "" {
		return anyResource
	}
	return handlers_ecs.ContainerInstanceARN(region, accountID, cluster, id)
}

func capacityProviderARN(region, accountID, ref string) string {
	name := handlers_ecs.CapacityProviderShortName(ref)
	if region == "" || accountID == "" || name == "" {
		return anyResource
	}
	return handlers_ecs.CapacityProviderARN(region, accountID, name)
}

// taskDefARN resolves "family", "family:revision" or a full ARN the way the
// handler does. A reference naming no revision wildcards it.
func taskDefARN(region, accountID, ref string) string {
	if ref == "" || region == "" || accountID == "" {
		return anyResource
	}
	family, rev := handlers_ecs.ParseTaskDefRef(ref)
	if family == "" {
		return anyResource
	}
	if rev <= 0 {
		return handlers_ecs.TaskDefRefARN(region, accountID, family, anyRevision)
	}
	return handlers_ecs.TaskDefARN(region, accountID, family, rev)
}

// tagARN re-anchors the caller-supplied resource ARN on gw.Region and the
// caller's account, because the tag handler ignores those segments and operates
// in the caller's own account bucket.
func tagARN(region, accountID, resourceARN string) string {
	if region == "" || accountID == "" {
		return anyResource
	}
	parts := strings.SplitN(resourceARN, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "ecs" {
		return anyResource
	}
	kind, resource, found := strings.Cut(parts[5], "/")
	if !found || resource == "" {
		return anyResource
	}
	switch kind {
	case "cluster", "task-definition", "service", "task", "container-instance":
		return "arn:aws:ecs:" + region + ":" + accountID + ":" + parts[5]
	default:
		// An ARN the tag handler rejects stays its own validation fault.
		return anyResource
	}
}
