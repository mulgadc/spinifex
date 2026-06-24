package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupECSRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	ctx := context.WithValue(req.Context(), ctxService, "ecs")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	return req.WithContext(ctx)
}

func TestECSActionFromTarget(t *testing.T) {
	assert.Equal(t, "RunTask",
		ecsActionFromTarget("AmazonEC2ContainerServiceV20141113.RunTask"))
	assert.Equal(t, "CreateCluster", ecsActionFromTarget("CreateCluster"))
	assert.Equal(t, "", ecsActionFromTarget(""))
}

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
		_, ok := gateway_ecs.Actions[action]
		assert.True(t, ok, "action %q should be registered", action)
	}
}

func TestECSRequest_MissingTarget(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECS_Request(httptest.NewRecorder(), setupECSRequest("", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestECSRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECS_Request(httptest.NewRecorder(),
		setupECSRequest("AmazonEC2ContainerServiceV20141113.MadeUpAction", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// An action whose real handler has not yet landed still resolves to the 501 stub.
func TestECSRequest_KnownActionNotImplemented(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECS_Request(httptest.NewRecorder(),
		setupECSRequest("AmazonEC2ContainerServiceV20141113.DeleteCluster", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// A request that clears auth but carries no account ID in context is an internal
// fault, not a client error.
func TestECSRequest_MissingAccountID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerServiceV20141113.ListClusters")
	req = req.WithContext(context.WithValue(req.Context(), ctxService, "ecs"))

	gw := &GatewayConfig{DisableLogging: true}
	err := gw.ECS_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
