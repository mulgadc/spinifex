package handlers_ecs

import (
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

func (s *NATSECSService) CreateCluster(input *ecs.CreateClusterInput, accountID string) (*ecs.CreateClusterOutput, error) {
	return utils.NATSRequest[ecs.CreateClusterOutput](s.natsConn, "ecs.CreateCluster", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeleteCluster(input *ecs.DeleteClusterInput, accountID string) (*ecs.DeleteClusterOutput, error) {
	return utils.NATSRequest[ecs.DeleteClusterOutput](s.natsConn, "ecs.DeleteCluster", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeClusters(input *ecs.DescribeClustersInput, accountID string) (*ecs.DescribeClustersOutput, error) {
	return utils.NATSRequest[ecs.DescribeClustersOutput](s.natsConn, "ecs.DescribeClusters", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListClusters(input *ecs.ListClustersInput, accountID string) (*ecs.ListClustersOutput, error) {
	return utils.NATSRequest[ecs.ListClustersOutput](s.natsConn, "ecs.ListClusters", input, defaultTimeout, accountID)
}

// --- Task definition ---

func (s *NATSECSService) RegisterTaskDefinition(input *ecs.RegisterTaskDefinitionInput, accountID string) (*ecs.RegisterTaskDefinitionOutput, error) {
	return utils.NATSRequest[ecs.RegisterTaskDefinitionOutput](s.natsConn, "ecs.RegisterTaskDefinition", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeregisterTaskDefinition(input *ecs.DeregisterTaskDefinitionInput, accountID string) (*ecs.DeregisterTaskDefinitionOutput, error) {
	return utils.NATSRequest[ecs.DeregisterTaskDefinitionOutput](s.natsConn, "ecs.DeregisterTaskDefinition", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeTaskDefinition(input *ecs.DescribeTaskDefinitionInput, accountID string) (*ecs.DescribeTaskDefinitionOutput, error) {
	return utils.NATSRequest[ecs.DescribeTaskDefinitionOutput](s.natsConn, "ecs.DescribeTaskDefinition", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListTaskDefinitions(input *ecs.ListTaskDefinitionsInput, accountID string) (*ecs.ListTaskDefinitionsOutput, error) {
	return utils.NATSRequest[ecs.ListTaskDefinitionsOutput](s.natsConn, "ecs.ListTaskDefinitions", input, defaultTimeout, accountID)
}

// --- Container instance ---

func (s *NATSECSService) RegisterContainerInstance(input *ecs.RegisterContainerInstanceInput, accountID string) (*ecs.RegisterContainerInstanceOutput, error) {
	return utils.NATSRequest[ecs.RegisterContainerInstanceOutput](s.natsConn, "ecs.RegisterContainerInstance", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeregisterContainerInstance(input *ecs.DeregisterContainerInstanceInput, accountID string) (*ecs.DeregisterContainerInstanceOutput, error) {
	return utils.NATSRequest[ecs.DeregisterContainerInstanceOutput](s.natsConn, "ecs.DeregisterContainerInstance", input, defaultTimeout, accountID)
}

func (s *NATSECSService) UpdateContainerInstancesState(input *ecs.UpdateContainerInstancesStateInput, accountID string) (*ecs.UpdateContainerInstancesStateOutput, error) {
	return utils.NATSRequest[ecs.UpdateContainerInstancesStateOutput](s.natsConn, "ecs.UpdateContainerInstancesState", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeContainerInstances(input *ecs.DescribeContainerInstancesInput, accountID string) (*ecs.DescribeContainerInstancesOutput, error) {
	return utils.NATSRequest[ecs.DescribeContainerInstancesOutput](s.natsConn, "ecs.DescribeContainerInstances", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListContainerInstances(input *ecs.ListContainerInstancesInput, accountID string) (*ecs.ListContainerInstancesOutput, error) {
	return utils.NATSRequest[ecs.ListContainerInstancesOutput](s.natsConn, "ecs.ListContainerInstances", input, defaultTimeout, accountID)
}

// --- Task ---

func (s *NATSECSService) RunTask(input *ecs.RunTaskInput, accountID string) (*ecs.RunTaskOutput, error) {
	return utils.NATSRequest[ecs.RunTaskOutput](s.natsConn, "ecs.RunTask", input, defaultTimeout, accountID)
}

func (s *NATSECSService) StartTask(input *ecs.StartTaskInput, accountID string) (*ecs.StartTaskOutput, error) {
	return utils.NATSRequest[ecs.StartTaskOutput](s.natsConn, "ecs.StartTask", input, defaultTimeout, accountID)
}

func (s *NATSECSService) StopTask(input *ecs.StopTaskInput, accountID string) (*ecs.StopTaskOutput, error) {
	return utils.NATSRequest[ecs.StopTaskOutput](s.natsConn, "ecs.StopTask", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeTasks(input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error) {
	return utils.NATSRequest[ecs.DescribeTasksOutput](s.natsConn, "ecs.DescribeTasks", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListTasks(input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error) {
	return utils.NATSRequest[ecs.ListTasksOutput](s.natsConn, "ecs.ListTasks", input, defaultTimeout, accountID)
}

func (s *NATSECSService) SubmitTaskStateChange(input *ecs.SubmitTaskStateChangeInput, accountID string) (*ecs.SubmitTaskStateChangeOutput, error) {
	return utils.NATSRequest[ecs.SubmitTaskStateChangeOutput](s.natsConn, "ecs.SubmitTaskStateChange", input, defaultTimeout, accountID)
}

// --- Service ---

func (s *NATSECSService) CreateService(input *ecs.CreateServiceInput, accountID string) (*ecs.CreateServiceOutput, error) {
	return utils.NATSRequest[ecs.CreateServiceOutput](s.natsConn, "ecs.CreateService", input, defaultTimeout, accountID)
}

func (s *NATSECSService) UpdateService(input *ecs.UpdateServiceInput, accountID string) (*ecs.UpdateServiceOutput, error) {
	return utils.NATSRequest[ecs.UpdateServiceOutput](s.natsConn, "ecs.UpdateService", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DeleteService(input *ecs.DeleteServiceInput, accountID string) (*ecs.DeleteServiceOutput, error) {
	return utils.NATSRequest[ecs.DeleteServiceOutput](s.natsConn, "ecs.DeleteService", input, defaultTimeout, accountID)
}

func (s *NATSECSService) DescribeServices(input *ecs.DescribeServicesInput, accountID string) (*ecs.DescribeServicesOutput, error) {
	return utils.NATSRequest[ecs.DescribeServicesOutput](s.natsConn, "ecs.DescribeServices", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListServices(input *ecs.ListServicesInput, accountID string) (*ecs.ListServicesOutput, error) {
	return utils.NATSRequest[ecs.ListServicesOutput](s.natsConn, "ecs.ListServices", input, defaultTimeout, accountID)
}

func (s *NATSECSService) PollAssignments(input *PollAssignmentsInput, accountID string) (*PollAssignmentsOutput, error) {
	return utils.NATSRequest[PollAssignmentsOutput](s.natsConn, "ecs.PollAssignments", input, defaultTimeout, accountID)
}

// --- Capacity ---

func (s *NATSECSService) ProvisionCapacity(input *ProvisionCapacityInput, accountID string) (*ProvisionCapacityOutput, error) {
	return utils.NATSRequest[ProvisionCapacityOutput](s.natsConn, "ecs.ProvisionCapacity", input, defaultTimeout, accountID)
}
