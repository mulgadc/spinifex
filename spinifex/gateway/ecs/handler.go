// Package gateway_ecs is the HTTP-side glue for the ECS control plane: the AWS
// JSON 1.1 surface dispatched by X-Amz-Target
// (AmazonEC2ContainerServiceV20141113.<Action>), matching aws-sdk-go's
// service/ecs request shape.
//
// The full v1 action namespace (ecs-v1.md §1) is registered as the API contract.
// Cluster, task-definition, container-instance and task actions dispatch to the
// daemon over NATS; the remaining actions resolve to the NotImplemented stub.
package gateway_ecs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// TargetPrefix is the X-Amz-Target service prefix for ECS control-plane calls.
const TargetPrefix = "AmazonEC2ContainerServiceV20141113"

// JSONContentType is the AWS JSON 1.1 content type ECS clients expect.
const JSONContentType = "application/x-amz-json-1.1"

// Handler is the signature every ECS control-plane action implements. nc is the
// gateway NATS connection (handlers relay onto ecs.* subjects); accountID is the
// resolved caller account; body is the raw JSON 1.1 request payload.
type Handler func(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error)

// NotImplemented is the placeholder for every unimplemented ECS control-plane
// action. It returns the AWS NotImplemented error, which the shared gateway
// ErrorHandler renders as a 501 JSON 1.1 envelope.
func NotImplemented(_ context.Context, _ *nats.Conn, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// Actions is the authoritative ECS control-plane action namespace (ecs-v1.md §1).
// Wired actions point at their handler; unimplemented actions keep the
// NotImplemented value. The key set is the v1 API contract.
var Actions = map[string]Handler{
	// Cluster. PutClusterCapacityProviders is a no-op stub in v1 (Q15).
	"CreateCluster":               CreateCluster,
	"DescribeClusters":            DescribeClusters,
	"ListClusters":                ListClusters,
	"DeleteCluster":               DeleteCluster,
	"UpdateCluster":               NotImplemented,
	"PutClusterCapacityProviders": NotImplemented,

	// Task definition.
	"RegisterTaskDefinition":     RegisterTaskDefinition,
	"DeregisterTaskDefinition":   DeregisterTaskDefinition,
	"DescribeTaskDefinition":     DescribeTaskDefinition,
	"ListTaskDefinitions":        ListTaskDefinitions,
	"ListTaskDefinitionFamilies": NotImplemented,

	// Task.
	"RunTask":       RunTask,
	"StartTask":     StartTask,
	"StopTask":      StopTask,
	"DescribeTasks": DescribeTasks,
	"ListTasks":     ListTasks,

	// Agent task-state reporting (agent → gateway → recordTaskState).
	"SubmitTaskStateChange": SubmitTaskStateChange,

	// Agent assignment polling (agent drains its inbox over the gateway).
	"PollAssignments": PollAssignments,

	// Service. ListServicesByNamespace is a no-op stub in v1.
	"CreateService":           CreateService,
	"UpdateService":           UpdateService,
	"DeleteService":           DeleteService,
	"DescribeServices":        DescribeServices,
	"ListServices":            ListServices,
	"ListServicesByNamespace": NotImplemented,

	// Container instance.
	"RegisterContainerInstance":     RegisterContainerInstance,
	"DeregisterContainerInstance":   DeregisterContainerInstance,
	"ProvisionCapacity":             ProvisionCapacity,
	"DescribeContainerInstances":    DescribeContainerInstances,
	"ListContainerInstances":        ListContainerInstances,
	"UpdateContainerInstancesState": UpdateContainerInstancesState,

	// Account settings (passthrough; no enforcement v1).
	"PutAccountSetting":   NotImplemented,
	"ListAccountSettings": NotImplemented,

	// Tags.
	"TagResource":         TagResource,
	"UntagResource":       UntagResource,
	"ListTagsForResource": ListTagsForResource,
}

// RawJSONActions encode their response with encoding/json instead of jsonutil.
// These are internal agent↔gateway shapes (not AWS SDK types) whose payloads
// carry RFC3339 time fields the on-VM agent decodes with encoding/json; the
// jsonutil marshaler would emit epoch-second floats and break the decode.
var RawJSONActions = map[string]bool{
	"PollAssignments": true,
}

// WriteJSONResponse serialises obj as a 200 AWS JSON 1.1 response using the
// SDK's jsonutil marshaler (epoch-second time encoding), matching ECR/ACM/EKS.
func WriteJSONResponse(w http.ResponseWriter, obj any) {
	body, err := jsonutil.BuildJSON(obj)
	if err != nil {
		slog.Error("ECS: failed to marshal response JSON", "err", err)
		http.Error(w, awserrors.ErrorInternalError, http.StatusInternalServerError)
		return
	}
	writeJSONBody(w, body)
}

// WriteRawJSONResponse serialises obj with encoding/json (RFC3339 times) for the
// internal agent↔gateway actions in RawJSONActions.
func WriteRawJSONResponse(w http.ResponseWriter, obj any) {
	body, err := json.Marshal(obj)
	if err != nil {
		slog.Error("ECS: failed to marshal raw response JSON", "err", err)
		http.Error(w, awserrors.ErrorInternalError, http.StatusInternalServerError)
		return
	}
	writeJSONBody(w, body)
}

func writeJSONBody(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECS: failed to write response", "err", err)
	}
}
