package awsgw_test

import (
	"testing"

	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	"github.com/stretchr/testify/assert"
)

// The Actions map is the v1 API contract (ecs-v1.md §1): every action across the
// Cluster / TaskDefinition / Task / Service / ContainerInstance / Account / Tags
// surfaces must be registered, whether wired to the daemon or a 501 stub.
func TestECSActionsMap_NamespaceRegistered(t *testing.T) {
	namespace := []string{
		// Cluster
		"CreateCluster", "DescribeClusters", "ListClusters", "DeleteCluster",
		"UpdateCluster", "PutClusterCapacityProviders",
		// Task definition
		"RegisterTaskDefinition", "DeregisterTaskDefinition", "DescribeTaskDefinition",
		"ListTaskDefinitions", "ListTaskDefinitionFamilies",
		// Task
		"RunTask", "StartTask", "StopTask", "DescribeTasks", "ListTasks",
		// Service
		"CreateService", "UpdateService", "DeleteService", "DescribeServices",
		"ListServices", "ListServicesByNamespace",
		// Container instance
		"RegisterContainerInstance", "DeregisterContainerInstance",
		"DescribeContainerInstances", "ListContainerInstances",
		"UpdateContainerInstancesState",
		// Account
		"PutAccountSetting", "ListAccountSettings",
		// Tags
		"TagResource", "UntagResource", "ListTagsForResource",
	}
	for _, action := range namespace {
		_, ok := ecsawsapi.Actions[action]
		assert.True(t, ok, "action %q should be registered", action)
	}
}
