package gateway_ecs

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go/service/ecs"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/nats-io/nats.go"
)

// unmarshalIfBody decodes body into out only when non-empty.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// --- Cluster ---

func CreateCluster(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.CreateClusterInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).CreateCluster(ctx, input, accountID)
}

func DeleteCluster(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DeleteClusterInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DeleteCluster(ctx, input, accountID)
}

func DescribeClusters(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeClustersInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeClusters(ctx, input, accountID)
}

func ListClusters(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListClustersInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListClusters(ctx, input, accountID)
}

// --- Task definition ---

func RegisterTaskDefinition(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.RegisterTaskDefinitionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).RegisterTaskDefinition(ctx, input, accountID)
}

func DeregisterTaskDefinition(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DeregisterTaskDefinitionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DeregisterTaskDefinition(ctx, input, accountID)
}

func DescribeTaskDefinition(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeTaskDefinitionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeTaskDefinition(ctx, input, accountID)
}

func ListTaskDefinitions(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListTaskDefinitionsInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListTaskDefinitions(ctx, input, accountID)
}

// --- Container instance ---

func RegisterContainerInstance(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.RegisterContainerInstanceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).RegisterContainerInstance(ctx, input, accountID)
}

// ProvisionCapacity launches container-instance EC2 capacity into a cluster.
// Input/output are the custom handlers_ecs types, not aws-sdk-go ecs shapes.
func ProvisionCapacity(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(handlers_ecs.ProvisionCapacityInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ProvisionCapacity(ctx, input, accountID)
}

func DeregisterContainerInstance(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DeregisterContainerInstanceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DeregisterContainerInstance(ctx, input, accountID)
}

func UpdateContainerInstancesState(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.UpdateContainerInstancesStateInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).UpdateContainerInstancesState(ctx, input, accountID)
}

func DescribeContainerInstances(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeContainerInstancesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeContainerInstances(ctx, input, accountID)
}

func ListContainerInstances(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListContainerInstancesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListContainerInstances(ctx, input, accountID)
}

// --- Task ---

func RunTask(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.RunTaskInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).RunTask(ctx, input, accountID)
}

func StartTask(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.StartTaskInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).StartTask(ctx, input, accountID)
}

func StopTask(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.StopTaskInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).StopTask(ctx, input, accountID)
}

func DescribeTasks(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeTasksInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeTasks(ctx, input, accountID)
}

func ListTasks(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListTasksInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListTasks(ctx, input, accountID)
}

// --- Service ---

func CreateService(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.CreateServiceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).CreateService(ctx, input, accountID)
}

func UpdateService(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.UpdateServiceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).UpdateService(ctx, input, accountID)
}

func DeleteService(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DeleteServiceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DeleteService(ctx, input, accountID)
}

func DescribeServices(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeServicesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeServices(ctx, input, accountID)
}

func ListServices(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListServicesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListServices(ctx, input, accountID)
}

// SubmitTaskStateChange is the agent's task-state report path over the gateway
// (replaces the Layer-2 bus publish). The account is the SigV4 caller, so an
// instance cannot report state for another account's task.
func SubmitTaskStateChange(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.SubmitTaskStateChangeInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).SubmitTaskStateChange(ctx, input, accountID)
}

// PollAssignments drains the calling instance's assignment inbox (replaces the
// Layer-2 assign subscribe). Internal agent↔gateway action, not an AWS SDK
// shape; the response carries bus.Assign with an RFC3339 time, so the gateway
// encodes it with encoding/json (RawJSONActions), not the jsonutil marshaler.
func PollAssignments(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(handlers_ecs.PollAssignmentsInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).PollAssignments(ctx, input, accountID)
}

// ReportTaskGPU carries the agent's local ledger's per-task GPU device
// assignment (agent → gateway). Internal action, not an AWS SDK shape: real
// AWS's SubmitTaskStateChange has no gpuIds field.
func ReportTaskGPU(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(handlers_ecs.ReportTaskGPUInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ReportTaskGPU(ctx, input, accountID)
}

// --- Tags ---

func TagResource(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.TagResourceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).TagResource(ctx, input, accountID)
}

func UntagResource(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.UntagResourceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).UntagResource(ctx, input, accountID)
}

func ListTagsForResource(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListTagsForResourceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListTagsForResource(ctx, input, accountID)
}

// --- Capacity providers ---

func PutClusterCapacityProviders(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.PutClusterCapacityProvidersInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).PutClusterCapacityProviders(ctx, input, accountID)
}

func CreateCapacityProvider(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.CreateCapacityProviderInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).CreateCapacityProvider(ctx, input, accountID)
}

func DescribeCapacityProviders(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeCapacityProvidersInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeCapacityProviders(ctx, input, accountID)
}

func DeleteCapacityProvider(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DeleteCapacityProviderInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DeleteCapacityProvider(ctx, input, accountID)
}
