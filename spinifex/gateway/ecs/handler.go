// Package gateway_ecs is the HTTP-side glue for the ECS control plane: the AWS
// JSON 1.1 surface dispatched by X-Amz-Target
// (AmazonEC2ContainerServiceV20141113.<Action>), matching aws-sdk-go's
// service/ecs request shape.
//
// The full v1 action namespace (ecs-v1.md §1) is registered as the API contract;
// every action resolves to the shared NotImplemented stub until its real handler
// lands in a later Phase 4 sprint.
package gateway_ecs

import (
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
type Handler func(nc *nats.Conn, accountID string, body []byte) (any, error)

// NotImplemented is the placeholder for every unimplemented ECS control-plane
// action. It returns the AWS NotImplemented error, which the shared gateway
// ErrorHandler renders as a 501 JSON 1.1 envelope.
func NotImplemented(_ *nats.Conn, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// Actions is the authoritative ECS control-plane action namespace (ecs-v1.md §1).
// Real implementations replace the NotImplemented value as each action lands; the
// key set is the v1 API contract.
var Actions = map[string]Handler{
	// Cluster. PutClusterCapacityProviders is a no-op stub in v1 (Q15).
	"CreateCluster":               NotImplemented,
	"DescribeClusters":            NotImplemented,
	"ListClusters":                NotImplemented,
	"DeleteCluster":               NotImplemented,
	"UpdateCluster":               NotImplemented,
	"PutClusterCapacityProviders": NotImplemented,

	// Task definition.
	"RegisterTaskDefinition":     NotImplemented,
	"DeregisterTaskDefinition":   NotImplemented,
	"DescribeTaskDefinition":     NotImplemented,
	"ListTaskDefinitions":        NotImplemented,
	"ListTaskDefinitionFamilies": NotImplemented,

	// Task.
	"RunTask":       NotImplemented,
	"StartTask":     NotImplemented,
	"StopTask":      NotImplemented,
	"DescribeTasks": NotImplemented,
	"ListTasks":     NotImplemented,

	// Service. ListServicesByNamespace is a no-op stub in v1.
	"CreateService":           NotImplemented,
	"UpdateService":           NotImplemented,
	"DeleteService":           NotImplemented,
	"DescribeServices":        NotImplemented,
	"ListServices":            NotImplemented,
	"ListServicesByNamespace": NotImplemented,

	// Container instance.
	"RegisterContainerInstance":     NotImplemented,
	"DeregisterContainerInstance":   NotImplemented,
	"DescribeContainerInstances":    NotImplemented,
	"ListContainerInstances":        NotImplemented,
	"UpdateContainerInstancesState": NotImplemented,

	// Account settings (passthrough; no enforcement v1).
	"PutAccountSetting":   NotImplemented,
	"ListAccountSettings": NotImplemented,

	// Tags.
	"TagResource":         NotImplemented,
	"UntagResource":       NotImplemented,
	"ListTagsForResource": NotImplemented,
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
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECS: failed to write response", "err", err)
	}
}
