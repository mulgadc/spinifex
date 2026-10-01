package awsapi

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// RegistryActionService composes the OCI registry capabilities needed by the
// ECR JSON image actions. It owns no storage wiring: composition supplies the
// narrow capabilities, while this adapter retains the AWS request, response
// and error semantics for each action.
//
// It is deliberately limited to registry-backed image actions. Repository
// metadata and lifecycle-policy actions have different backing capabilities
// and remain separate compositions rather than becoming one broad ECR service.
type RegistryActionService struct {
	catalog ImageCatalog
	reader  ManifestReader
	writer  ManifestWriter
	deleter ImageDeleter
}

// NewRegistryActionService binds the concrete OCI registry capabilities used
// by the ECR JSON image-action surface. A Registry commonly implements all
// four interfaces, but accepting them separately keeps the dependency boundary
// explicit and permits independent capability implementations in tests.
func NewRegistryActionService(catalog ImageCatalog, reader ManifestReader, writer ManifestWriter, deleter ImageDeleter) *RegistryActionService {
	return &RegistryActionService{
		catalog: catalog,
		reader:  reader,
		writer:  writer,
		deleter: deleter,
	}
}

var registryActionNames = []string{
	"BatchDeleteImage",
	"BatchGetImage",
	"DescribeImages",
	"ListImages",
	"PutImage",
}

// RegistryActionNames returns the ECR control-plane actions served by a
// RegistryActionService. The copy prevents a coverage consumer from changing
// the composed action inventory.
func RegistryActionNames() []string {
	return append([]string(nil), registryActionNames...)
}

// IsRegistryAction reports whether action is served through the composed OCI
// registry capability rather than the NATS action table or a transitional
// HTTP adapter.
func IsRegistryAction(action string) bool {
	for _, name := range registryActionNames {
		if action == name {
			return true
		}
	}
	return false
}

// Execute handles a registry-backed ECR JSON action using the caller account
// already established by gateway authentication and policy enforcement.
func (s *RegistryActionService) Execute(ctx context.Context, action, accountID string, body []byte) (any, error) {
	if s == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	switch action {
	case "ListImages":
		return ListImages(ctx, s.catalog, accountID, body)
	case "DescribeImages":
		return DescribeImages(ctx, s.catalog, accountID, body)
	case "BatchGetImage":
		return BatchGetImage(ctx, s.reader, accountID, body)
	case "PutImage":
		return PutImage(ctx, s.writer, accountID, body)
	case "BatchDeleteImage":
		return BatchDeleteImage(ctx, s.deleter, accountID, body)
	default:
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
}
