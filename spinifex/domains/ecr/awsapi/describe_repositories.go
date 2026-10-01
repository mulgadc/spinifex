package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// describeRepositoriesRequest is the camelCase AWS JSON 1.1 input shape. The
// SDK input struct carries locationName tags rather than JSON tags, so the
// subset Spinifex honours is decoded explicitly.
type describeRepositoriesRequest struct {
	RegistryID      string   `json:"registryId"`
	RepositoryNames []string `json:"repositoryNames"`
}

// DescribeRepositories lists repositories visible in the caller's account.
// A registryId for a different account is denied until registry-policy v2
// implements the Q8 cross-account policy model. Pagination (Q9) is not yet
// implemented, so an unfiltered request returns the complete local list.
//
// The caller supplies the advertised endpoint profile because it is deployment
// composition, rather than ECR resource state.
func DescribeRepositories(ctx context.Context, nc *nats.Conn, endpoint RepositoryEndpoint, accountID string, body []byte) (*ecr.DescribeRepositoriesOutput, error) {
	var req describeRepositoriesRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, MalformedBodyError()
		}
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	store := handlers_ecr.NewNATSMetaStore(nc)
	names := req.RepositoryNames
	if len(names) == 0 {
		var err error
		names, err = store.ListRepos(ctx, accountID)
		if err != nil {
			slog.ErrorContext(ctx, "ECR DescribeRepositories: list repositories failed", "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	repositories := make([]*ecr.Repository, 0, len(names))
	for _, name := range names {
		meta, err := store.GetRepo(ctx, accountID, name)
		if err != nil {
			if errors.Is(err, handlers_ecr.ErrNotFound) {
				return nil, errors.New(awserrors.ErrorRepositoryNotFound)
			}
			slog.ErrorContext(ctx, "ECR DescribeRepositories: get repository failed", "repository", name, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		repositories = append(repositories, endpoint.RepositoryFromMeta(accountID, name, meta))
	}

	return &ecr.DescribeRepositoriesOutput{Repositories: repositories}, nil
}
