package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ecr"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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
func DeleteRepository(ctx context.Context, store RepositoryStore, endpoint RepositoryEndpoint, accountID string, body []byte) (*ecr.DeleteRepositoryOutput, error) {
	if store == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
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

	meta, err := store.GetRepo(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, ecrdomain.ErrNotFound) {
			return nil, RepositoryNotFoundError(accountID, req.RepositoryName)
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
		if errors.Is(err, ecrdomain.ErrNotFound) {
			return nil, RepositoryNotFoundError(accountID, req.RepositoryName)
		}
		slog.ErrorContext(ctx, "ECR DeleteRepository: delete repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// AWS's DeleteRepository omits the encryption and scanning configurations
	// that every other repository response carries.
	repo := endpoint.RepositoryFromMeta(accountID, req.RepositoryName, meta)
	repo.EncryptionConfiguration = nil
	repo.ImageScanningConfiguration = nil
	return &ecr.DeleteRepositoryOutput{Repository: repo}, nil
}
