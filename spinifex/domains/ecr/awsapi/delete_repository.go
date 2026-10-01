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

// deleteRepositoryRequest is the camelCase AWS JSON 1.1 input shape. force
// permits deletion when a repository still contains images.
type deleteRepositoryRequest struct {
	RepositoryName string `json:"repositoryName"`
	RegistryID     string `json:"registryId"`
	Force          bool   `json:"force"`
}

// DeleteRepository removes an account-scoped ECR repository. Without force,
// a repository with image manifests returns RepositoryNotEmptyException. The
// metadata service cascades repository records; object blob reclamation is
// intentionally deferred to the separate garbage-collection path.
func DeleteRepository(ctx context.Context, nc *nats.Conn, endpoint RepositoryEndpoint, accountID string, body []byte) (*ecr.DeleteRepositoryOutput, error) {
	var req deleteRepositoryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryName(req.RepositoryName); err != nil {
		return nil, err
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	store := handlers_ecr.NewNATSMetaStore(nc)
	meta, err := store.GetRepo(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		slog.ErrorContext(ctx, "ECR DeleteRepository: get repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if !req.Force {
		manifests, err := store.ListManifests(ctx, accountID, req.RepositoryName)
		if err != nil {
			slog.ErrorContext(ctx, "ECR DeleteRepository: list manifests failed", "repository", req.RepositoryName, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if len(manifests) > 0 {
			return nil, errors.New(awserrors.ErrorRepositoryNotEmpty)
		}
	}

	if err := store.DeleteRepo(ctx, accountID, req.RepositoryName); err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		slog.ErrorContext(ctx, "ECR DeleteRepository: delete repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &ecr.DeleteRepositoryOutput{
		Repository: endpoint.RepositoryFromMeta(accountID, req.RepositoryName, meta),
	}, nil
}
