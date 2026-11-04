// Package awsapi owns ECR's AWS JSON 1.1 action surface: the
// X-Amz-Target action inventory, request validation, resource-scope derivation
// and NATS-backed action handlers. It matches aws-sdk-go's service/ecr request
// shape. Generic gateway code retains HTTP routing, authentication context and
// policy enforcement; the ECR OCI Distribution registry is a separate adapter.
//
// The full action namespace is registered as the API contract; every action
// resolves to the shared NotImplemented stub until its real handler lands.
package awsapi

import (
	"context"
	"errors"
	"reflect"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// TargetPrefix is the X-Amz-Target service prefix for ECR control-plane calls.
const TargetPrefix = "AmazonEC2ContainerRegistry_V20150921"

// Handler is the signature every ECR control-plane action implements. nc is the
// gateway NATS connection (handlers relay onto ecr.* subjects); accountID is the
// resolved caller account; body is the raw JSON 1.1 request payload.
type Handler func(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error)

// NotImplemented is the placeholder for every unimplemented ECR control-plane
// action. It returns the AWS NotImplemented error, which the shared gateway
// ErrorHandler renders as a 501 JSON 1.1 envelope.
func NotImplemented(_ context.Context, _ *nats.Conn, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// ScanningNotSupported is the handler for the ECR image-scanning surface. mulga
// runs no vulnerability scanner, so these actions return the AWS
// OperationNotSupportedException (400) rather than NotImplemented (501): clients
// and the console read it as a deliberately-off feature, not a server fault.
func ScanningNotSupported(_ context.Context, _ *nats.Conn, _ string, _ []byte) (any, error) {
	return nil, errors.New(awserrors.ErrorOperationNotSupported)
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
	"PutImageTagMutability": PutImageTagMutability,

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
	"SetRepositoryPolicy":    SetRepositoryPolicy,
	"GetRepositoryPolicy":    GetRepositoryPolicy,
	"DeleteRepositoryPolicy": DeleteRepositoryPolicy,
	"GetRegistryPolicy":      NotImplemented,
	"PutRegistryPolicy":      NotImplemented,
	"DescribeRegistry":       DescribeRegistry,

	// Image scanning is unsupported (no scanner backend), except that
	// PutImageScanningConfiguration accepts the scanOnPush=false no-op rather
	// than rejecting the whole action unread.
	"PutImageScanningConfiguration":           PutImageScanningConfiguration,
	"GetImageScanningConfiguration":           ScanningNotSupported,
	"StartImageScan":                          ScanningNotSupported,
	"DescribeImageScanFindings":               ScanningNotSupported,
	"GetRegistryScanningConfiguration":        ScanningNotSupported,
	"PutRegistryScanningConfiguration":        ScanningNotSupported,
	"BatchGetRepositoryScanningConfiguration": ScanningNotSupported,

	// Deferred-feature surface (lifecycle, replication).
	"PutLifecyclePolicy":          PutLifecyclePolicy,
	"GetLifecyclePolicy":          GetLifecyclePolicy,
	"DeleteLifecyclePolicy":       DeleteLifecyclePolicy,
	"StartLifecyclePolicyPreview": NotImplemented,
	"GetLifecyclePolicyPreview":   NotImplemented,
	"PutReplicationConfiguration": NotImplemented,
	"ReplicateImage":              NotImplemented,

	// Resource tags.
	"TagResource":         TagResource,
	"UntagResource":       UntagResource,
	"ListTagsForResource": ListTagsForResource,
}

// StubbedActionNames returns the Actions entries still bound to NotImplemented.
func StubbedActionNames() []string {
	return actionsWithHandler(NotImplemented)
}

// UnsupportedActionNames returns actions that deliberately report the feature
// as unsupported rather than behaving as implemented operations.
func UnsupportedActionNames() []string {
	return actionsWithHandler(ScanningNotSupported)
}

func actionsWithHandler(target Handler) []string {
	want := reflect.ValueOf(target).Pointer()
	actions := make([]string, 0)
	for name, handler := range Actions {
		if reflect.ValueOf(handler).Pointer() == want {
			actions = append(actions, name)
		}
	}
	sort.Strings(actions)
	return actions
}
