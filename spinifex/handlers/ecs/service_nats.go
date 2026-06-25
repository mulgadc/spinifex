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

func (s *NATSECSService) DescribeTasks(input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error) {
	return utils.NATSRequest[ecs.DescribeTasksOutput](s.natsConn, "ecs.DescribeTasks", input, defaultTimeout, accountID)
}

func (s *NATSECSService) ListTasks(input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error) {
	return utils.NATSRequest[ecs.ListTasksOutput](s.natsConn, "ecs.ListTasks", input, defaultTimeout, accountID)
}

func (s *NATSECSService) SubmitTaskStateChange(input *ecs.SubmitTaskStateChangeInput, accountID string) (*ecs.SubmitTaskStateChangeOutput, error) {
	return utils.NATSRequest[ecs.SubmitTaskStateChangeOutput](s.natsConn, "ecs.SubmitTaskStateChange", input, defaultTimeout, accountID)
}

func (s *NATSECSService) PollAssignments(input *PollAssignmentsInput, accountID string) (*PollAssignmentsOutput, error) {
	return utils.NATSRequest[PollAssignmentsOutput](s.natsConn, "ecs.PollAssignments", input, defaultTimeout, accountID)
}
