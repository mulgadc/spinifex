// Package gateway_ecrapi is the HTTP-side glue for the ECR control plane: the
// AWS JSON 1.1 surface dispatched by X-Amz-Target
// (AmazonEC2ContainerRegistry_V20150921.<Action>), matching aws-sdk-go's
// service/ecr request shape. It is distinct from package gateway_ecr, which
// serves the OCI Distribution registry (/v2/*).
//
// The full action namespace is registered as the API contract; every action
// resolves to the shared NotImplemented stub until its real handler lands.
package gateway_ecrapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// TargetPrefix is the X-Amz-Target service prefix for ECR control-plane calls.
const TargetPrefix = "AmazonEC2ContainerRegistry_V20150921"

// JSONContentType is the AWS JSON 1.1 content type ECR clients expect.
const JSONContentType = "application/x-amz-json-1.1"

// Handler is the signature every ECR control-plane action implements. nc is the
// gateway NATS connection (handlers relay onto ecr.* subjects); accountID is the
// resolved caller account; body is the raw JSON 1.1 request payload.
type Handler func(nc *nats.Conn, accountID string, body []byte) (any, error)

// NotImplemented is the placeholder for every unimplemented ECR control-plane
// action. It returns the AWS NotImplemented error, which the shared gateway
// ErrorHandler renders as a 501 JSON 1.1 envelope.
func NotImplemented(_ *nats.Conn, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// Actions is the authoritative ECR control-plane action namespace. Real
// implementations replace the NotImplemented value as each action lands; the
// key set is the v1 API contract.
var Actions = map[string]Handler{
	// Auth.
	"GetAuthorizationToken": NotImplemented,

	// Repositories.
	"CreateRepository":      NotImplemented,
	"DeleteRepository":      NotImplemented,
	"DescribeRepositories":  NotImplemented,
	"ListRepositories":      NotImplemented,
	"PutImageTagMutability": NotImplemented,

	// Images / layers.
	"BatchGetImage":               NotImplemented,
	"BatchCheckLayerAvailability": NotImplemented,
	"BatchDeleteImage":            NotImplemented,
	"PutImage":                    NotImplemented,
	"ListImages":                  NotImplemented,
	"DescribeImages":              NotImplemented,
	"GetDownloadUrlForLayer":      NotImplemented,
	"InitiateLayerUpload":         NotImplemented,
	"UploadLayerPart":             NotImplemented,
	"CompleteLayerUpload":         NotImplemented,

	// Repository / registry policy.
	"SetRepositoryPolicy":    NotImplemented,
	"GetRepositoryPolicy":    NotImplemented,
	"DeleteRepositoryPolicy": NotImplemented,
	"GetRegistryPolicy":      NotImplemented,
	"PutRegistryPolicy":      NotImplemented,
	"DescribeRegistry":       NotImplemented,

	// Deferred-feature surface (image scan, lifecycle, replication, tagging).
	"PutImageScanningConfiguration": NotImplemented,
	"GetImageScanningConfiguration": NotImplemented,
	"StartImageScan":                NotImplemented,
	"DescribeImageScanFindings":     NotImplemented,
	"PutLifecyclePolicy":            NotImplemented,
	"GetLifecyclePolicy":            NotImplemented,
	"DeleteLifecyclePolicy":         NotImplemented,
	"StartLifecyclePolicyPreview":   NotImplemented,
	"GetLifecyclePolicyPreview":     NotImplemented,
	"PutReplicationConfiguration":   NotImplemented,
	"ReplicateImage":                NotImplemented,
	"TagResource":                   NotImplemented,
	"UntagResource":                 NotImplemented,
	"ListTagsForResource":           NotImplemented,
}

// WriteJSONResponse serialises obj as a 200 AWS JSON 1.1 response using the
// SDK's jsonutil marshaler (epoch-second time encoding), matching ACM/EKS.
func WriteJSONResponse(w http.ResponseWriter, obj any) {
	body, err := jsonutil.BuildJSON(obj)
	if err != nil {
		slog.Error("ECR: failed to marshal response JSON", "err", err)
		http.Error(w, awserrors.ErrorInternalError, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECR: failed to write response", "err", err)
	}
}
