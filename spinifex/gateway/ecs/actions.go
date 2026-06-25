package gateway_ecs

import (
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

func CreateCluster(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.CreateClusterInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).CreateCluster(input, accountID)
}

func DescribeClusters(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeClustersInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeClusters(input, accountID)
}

func ListClusters(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListClustersInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListClusters(input, accountID)
}

// --- Task definition ---

func RegisterTaskDefinition(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.RegisterTaskDefinitionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).RegisterTaskDefinition(input, accountID)
}

func DescribeTaskDefinition(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeTaskDefinitionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeTaskDefinition(input, accountID)
}

func ListTaskDefinitions(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListTaskDefinitionsInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListTaskDefinitions(input, accountID)
}

// --- Container instance ---

func RegisterContainerInstance(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.RegisterContainerInstanceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).RegisterContainerInstance(input, accountID)
}

func DescribeContainerInstances(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeContainerInstancesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeContainerInstances(input, accountID)
}

func ListContainerInstances(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListContainerInstancesInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListContainerInstances(input, accountID)
}

// --- Task ---

func RunTask(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.RunTaskInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).RunTask(input, accountID)
}

func DescribeTasks(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.DescribeTasksInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).DescribeTasks(input, accountID)
}

func ListTasks(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.ListTasksInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).ListTasks(input, accountID)
}

// SubmitTaskStateChange is the agent's task-state report path over the gateway
// (replaces the Layer-2 bus publish). The account is the SigV4 caller, so an
// instance cannot report state for another account's task.
func SubmitTaskStateChange(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(ecs.SubmitTaskStateChangeInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).SubmitTaskStateChange(input, accountID)
}

// PollAssignments drains the calling instance's assignment inbox (replaces the
// Layer-2 assign subscribe). Internal agent↔gateway action, not an AWS SDK
// shape; the response carries bus.Assign with an RFC3339 time, so the gateway
// encodes it with encoding/json (RawJSONActions), not the jsonutil marshaler.
func PollAssignments(nc *nats.Conn, accountID string, body []byte) (any, error) {
	input := new(handlers_ecs.PollAssignmentsInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_ecs.NewNATSECSService(nc).PollAssignments(input, accountID)
}
