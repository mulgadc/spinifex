package awsapi

import (
	"context"
	"errors"
	"slices"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// RepositoryStore is the account-scoped metadata capability consumed by the
// repository action group. It deliberately omits policy, tag and upload
// operations: this control-plane surface needs only repository records and
// their manifest inventory.
type RepositoryStore interface {
	GetRepo(ctx context.Context, accountID, repo string) (handlers_ecr.RepoMeta, error)
	ListRepos(ctx context.Context, accountID string) ([]string, error)
	PutRepo(ctx context.Context, accountID string, meta handlers_ecr.RepoMeta) error
	ListManifests(ctx context.Context, accountID, repo string) ([]string, error)
	DeleteRepo(ctx context.Context, accountID, repo string) error
}

// RepositoryActionService composes the metadata store and advertised endpoint
// profile used by the ECR repository action group. The profile remains
// deployment composition, while AWS request validation and response projection
// remain in the individual action adapters.
type RepositoryActionService struct {
	store    RepositoryStore
	endpoint RepositoryEndpoint
}

// NewRepositoryActionService binds the repository metadata capability and the
// configured ECR endpoint profile for repository action dispatch.
func NewRepositoryActionService(store RepositoryStore, endpoint RepositoryEndpoint) *RepositoryActionService {
	return &RepositoryActionService{store: store, endpoint: endpoint}
}

var repositoryActionNames = []string{
	"CreateRepository",
	"DeleteRepository",
	"DescribeRepositories",
}

// RepositoryActionNames returns the repository actions served by
// RepositoryActionService. The copy keeps coverage consumers from changing the
// service's dispatch set.
func RepositoryActionNames() []string {
	return append([]string(nil), repositoryActionNames...)
}

// IsRepositoryAction reports whether action is handled by the composed
// repository-metadata capability.
func IsRepositoryAction(action string) bool {
	return slices.Contains(repositoryActionNames, action)
}

// Execute handles a repository ECR JSON action using the caller account
// established by gateway authentication and policy enforcement.
func (s *RepositoryActionService) Execute(ctx context.Context, action, accountID string, body []byte) (any, error) {
	if s == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	switch action {
	case "CreateRepository":
		return CreateRepository(ctx, s.store, s.endpoint, accountID, body)
	case "DeleteRepository":
		return DeleteRepository(ctx, s.store, s.endpoint, accountID, body)
	case "DescribeRepositories":
		return DescribeRepositories(ctx, s.store, s.endpoint, accountID, body)
	default:
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
}
