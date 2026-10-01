package awsapi

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// LifecyclePreviewActionService composes the policy-store and image-catalog
// capabilities used by the ECR lifecycle-preview actions. It deliberately does
// not share RegistryActionService: lifecycle previews have a different storage
// dependency and should not make the OCI registry a broad ECR control-plane
// service.
type LifecyclePreviewActionService struct {
	policies LifecyclePolicyStore
	catalog  ImageCatalog
}

// NewLifecyclePreviewActionService binds the policy and image capabilities
// consumed by the synchronous lifecycle-preview action pair.
func NewLifecyclePreviewActionService(policies LifecyclePolicyStore, catalog ImageCatalog) *LifecyclePreviewActionService {
	return &LifecyclePreviewActionService{policies: policies, catalog: catalog}
}

var lifecyclePreviewActionNames = []string{
	"GetLifecyclePolicyPreview",
	"StartLifecyclePolicyPreview",
}

// LifecyclePreviewActionNames returns the lifecycle-preview actions served by
// LifecyclePreviewActionService. The copy keeps the coverage inventory from
// changing the service's dispatch set.
func LifecyclePreviewActionNames() []string {
	return append([]string(nil), lifecyclePreviewActionNames...)
}

// IsLifecyclePreviewAction reports whether action is handled by the composed
// lifecycle-preview capability.
func IsLifecyclePreviewAction(action string) bool {
	for _, name := range lifecyclePreviewActionNames {
		if action == name {
			return true
		}
	}
	return false
}

// Execute handles a lifecycle-preview ECR JSON action using the caller account
// established by gateway authentication and policy enforcement.
func (s *LifecyclePreviewActionService) Execute(ctx context.Context, action, accountID string, body []byte) (any, error) {
	if s == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	switch action {
	case "StartLifecyclePolicyPreview":
		return StartLifecyclePolicyPreview(ctx, s.policies, s.catalog, accountID, body)
	case "GetLifecyclePolicyPreview":
		return GetLifecyclePolicyPreview(ctx, s.policies, s.catalog, accountID, body)
	default:
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
}
