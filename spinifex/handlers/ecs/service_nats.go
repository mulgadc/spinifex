package handlers_ecs

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// NATSECSService is the gateway-side adapter that forwards every ECSService
// method as a NATS request to the daemon's matching subscriber.
type NATSECSService struct {
	natsConn *nats.Conn
}

var _ ECSService = (*NATSECSService)(nil)

// NewNATSECSService returns an ECSService that uses NATS request-response.
func NewNATSECSService(conn *nats.Conn) ECSService {
	return &NATSECSService{natsConn: conn}
}

// --- Cluster ---

func (s *NATSECSService) CreateCluster(ctx context.Context, input *ecs.CreateClusterInput, accountID string) (*ecs.CreateClusterOutput, error) {
	return utils.NATSRequest[ecs.CreateClusterOutput](ctx, s.natsConn, "ecs.CreateCluster", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeleteCluster(ctx context.Context, input *ecs.DeleteClusterInput, accountID string) (*ecs.DeleteClusterOutput, error) {
	return utils.NATSRequest[ecs.DeleteClusterOutput](ctx, s.natsConn, "ecs.DeleteCluster", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeClusters(ctx context.Context, input *ecs.DescribeClustersInput, accountID string) (*ecs.DescribeClustersOutput, error) {
	return utils.NATSRequest[ecs.DescribeClustersOutput](ctx, s.natsConn, "ecs.DescribeClusters", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListClusters(ctx context.Context, input *ecs.ListClustersInput, accountID string) (*ecs.ListClustersOutput, error) {
	return utils.NATSRequest[ecs.ListClustersOutput](ctx, s.natsConn, "ecs.ListClusters", input, defaultTimeout, accountID)
}

// --- Task definition ---

func (s *NATSECSService) RegisterTaskDefinition(ctx context.Context, input *ecs.RegisterTaskDefinitionInput, accountID string) (*ecs.RegisterTaskDefinitionOutput, error) {
	return utils.NATSRequest[ecs.RegisterTaskDefinitionOutput](ctx, s.natsConn, "ecs.RegisterTaskDefinition", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeregisterTaskDefinition(ctx context.Context, input *ecs.DeregisterTaskDefinitionInput, accountID string) (*ecs.DeregisterTaskDefinitionOutput, error) {
	return utils.NATSRequest[ecs.DeregisterTaskDefinitionOutput](ctx, s.natsConn, "ecs.DeregisterTaskDefinition", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeTaskDefinition(ctx context.Context, input *ecs.DescribeTaskDefinitionInput, accountID string) (*ecs.DescribeTaskDefinitionOutput, error) {
	return utils.NATSRequest[ecs.DescribeTaskDefinitionOutput](ctx, s.natsConn, "ecs.DescribeTaskDefinition", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListTaskDefinitions(ctx context.Context, input *ecs.ListTaskDefinitionsInput, accountID string) (*ecs.ListTaskDefinitionsOutput, error) {
	return utils.NATSRequest[ecs.ListTaskDefinitionsOutput](ctx, s.natsConn, "ecs.ListTaskDefinitions", input, defaultTimeout, accountID)
}

// --- Container instance ---

func (s *NATSECSService) RegisterContainerInstance(ctx context.Context, input *ecs.RegisterContainerInstanceInput, accountID string) (*ecs.RegisterContainerInstanceOutput, error) {
	return utils.NATSRequest[ecs.RegisterContainerInstanceOutput](ctx, s.natsConn, "ecs.RegisterContainerInstance", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeregisterContainerInstance(ctx context.Context, input *ecs.DeregisterContainerInstanceInput, accountID string) (*ecs.DeregisterContainerInstanceOutput, error) {
	return utils.NATSRequest[ecs.DeregisterContainerInstanceOutput](ctx, s.natsConn, "ecs.DeregisterContainerInstance", input, defaultTimeout, accountID)
}

func (s *NATSECSService) UpdateContainerInstancesState(ctx context.Context, input *ecs.UpdateContainerInstancesStateInput, accountID string) (*ecs.UpdateContainerInstancesStateOutput, error) {
	return utils.NATSRequest[ecs.UpdateContainerInstancesStateOutput](ctx, s.natsConn, "ecs.UpdateContainerInstancesState", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeContainerInstances(ctx context.Context, input *ecs.DescribeContainerInstancesInput, accountID string) (*ecs.DescribeContainerInstancesOutput, error) {
	return utils.NATSRequest[ecs.DescribeContainerInstancesOutput](ctx, s.natsConn, "ecs.DescribeContainerInstances", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListContainerInstances(ctx context.Context, input *ecs.ListContainerInstancesInput, accountID string) (*ecs.ListContainerInstancesOutput, error) {
	return utils.NATSRequest[ecs.ListContainerInstancesOutput](ctx, s.natsConn, "ecs.ListContainerInstances", input, defaultTimeout, accountID)
}

// --- Task ---

func (s *NATSECSService) RunTask(ctx context.Context, input *ecs.RunTaskInput, accountID string) (*ecs.RunTaskOutput, error) {
	return utils.NATSRequest[ecs.RunTaskOutput](ctx, s.natsConn, "ecs.RunTask", input, defaultTimeout, accountID)
}

func (s *NATSECSService) StartTask(ctx context.Context, input *ecs.StartTaskInput, accountID string) (*ecs.StartTaskOutput, error) {
	return utils.NATSRequest[ecs.StartTaskOutput](ctx, s.natsConn, "ecs.StartTask", input, defaultTimeout, accountID)
}

func (s *NATSECSService) StopTask(ctx context.Context, input *ecs.StopTaskInput, accountID string) (*ecs.StopTaskOutput, error) {
	return utils.NATSRequest[ecs.StopTaskOutput](ctx, s.natsConn, "ecs.StopTask", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeTasks(ctx context.Context, input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error) {
	return utils.NATSRequest[ecs.DescribeTasksOutput](ctx, s.natsConn, "ecs.DescribeTasks", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListTasks(ctx context.Context, input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error) {
	return utils.NATSRequest[ecs.ListTasksOutput](ctx, s.natsConn, "ecs.ListTasks", input, defaultTimeout, accountID)
}

func (s *NATSECSService) SubmitTaskStateChange(ctx context.Context, input *ecs.SubmitTaskStateChangeInput, accountID string) (*ecs.SubmitTaskStateChangeOutput, error) {
	return utils.NATSRequest[ecs.SubmitTaskStateChangeOutput](ctx, s.natsConn, "ecs.SubmitTaskStateChange", input, defaultTimeout, accountID)
}

// --- Service ---

func (s *NATSECSService) CreateService(ctx context.Context, input *ecs.CreateServiceInput, accountID string) (*ecs.CreateServiceOutput, error) {
	return utils.NATSRequest[ecs.CreateServiceOutput](ctx, s.natsConn, "ecs.CreateService", input, defaultTimeout, accountID)
}

func (s *NATSECSService) UpdateService(ctx context.Context, input *ecs.UpdateServiceInput, accountID string) (*ecs.UpdateServiceOutput, error) {
	return utils.NATSRequest[ecs.UpdateServiceOutput](ctx, s.natsConn, "ecs.UpdateService", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeleteService(ctx context.Context, input *ecs.DeleteServiceInput, accountID string) (*ecs.DeleteServiceOutput, error) {
	return utils.NATSRequest[ecs.DeleteServiceOutput](ctx, s.natsConn, "ecs.DeleteService", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeServices(ctx context.Context, input *ecs.DescribeServicesInput, accountID string) (*ecs.DescribeServicesOutput, error) {
	return utils.NATSRequest[ecs.DescribeServicesOutput](ctx, s.natsConn, "ecs.DescribeServices", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListServices(ctx context.Context, input *ecs.ListServicesInput, accountID string) (*ecs.ListServicesOutput, error) {
	return utils.NATSRequest[ecs.ListServicesOutput](ctx, s.natsConn, "ecs.ListServices", input, defaultTimeout, accountID)
}

func (s *NATSECSService) PollAssignments(ctx context.Context, input *PollAssignmentsInput, accountID string) (*PollAssignmentsOutput, error) {
	return utils.NATSRequest[PollAssignmentsOutput](ctx, s.natsConn, "ecs.PollAssignments", input, defaultTimeout, accountID)
}

// --- Capacity ---

func (s *NATSECSService) ProvisionCapacity(ctx context.Context, input *ProvisionCapacityInput, accountID string) (*ProvisionCapacityOutput, error) {
	return utils.NATSRequest[ProvisionCapacityOutput](ctx, s.natsConn, "ecs.ProvisionCapacity", input, defaultTimeout, accountID)
}

// --- Tags ---

func (s *NATSECSService) ListTagsForResource(ctx context.Context, input *ecs.ListTagsForResourceInput, accountID string) (*ecs.ListTagsForResourceOutput, error) {
	return utils.NATSRequest[ecs.ListTagsForResourceOutput](ctx, s.natsConn, "ecs.ListTagsForResource", input, defaultTimeout, accountID)
}

func (s *NATSECSService) TagResource(ctx context.Context, input *ecs.TagResourceInput, accountID string) (*ecs.TagResourceOutput, error) {
	return utils.NATSRequest[ecs.TagResourceOutput](ctx, s.natsConn, "ecs.TagResource", input, defaultTimeout, accountID)
}

func (s *NATSECSService) UntagResource(ctx context.Context, input *ecs.UntagResourceInput, accountID string) (*ecs.UntagResourceOutput, error) {
	return utils.NATSRequest[ecs.UntagResourceOutput](ctx, s.natsConn, "ecs.UntagResource", input, defaultTimeout, accountID)
}

// --- Capacity providers ---

func (s *NATSECSService) PutClusterCapacityProviders(ctx context.Context, input *ecs.PutClusterCapacityProvidersInput, accountID string) (*ecs.PutClusterCapacityProvidersOutput, error) {
	return utils.NATSRequest[ecs.PutClusterCapacityProvidersOutput](ctx, s.natsConn, "ecs.PutClusterCapacityProviders", input, defaultTimeout, accountID)
}

func (s *NATSECSService) CreateCapacityProvider(ctx context.Context, input *ecs.CreateCapacityProviderInput, accountID string) (*ecs.CreateCapacityProviderOutput, error) {
	return utils.NATSRequest[ecs.CreateCapacityProviderOutput](ctx, s.natsConn, "ecs.CreateCapacityProvider", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeCapacityProviders(ctx context.Context, input *ecs.DescribeCapacityProvidersInput, accountID string) (*ecs.DescribeCapacityProvidersOutput, error) {
	return utils.NATSRequest[ecs.DescribeCapacityProvidersOutput](ctx, s.natsConn, "ecs.DescribeCapacityProviders", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeleteCapacityProvider(ctx context.Context, input *ecs.DeleteCapacityProviderInput, accountID string) (*ecs.DeleteCapacityProviderOutput, error) {
	return utils.NATSRequest[ecs.DeleteCapacityProviderOutput](ctx, s.natsConn, "ecs.DeleteCapacityProvider", input, defaultTimeout, accountID)
}
