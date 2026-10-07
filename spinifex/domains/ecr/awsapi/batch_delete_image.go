package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

type batchDeleteImageRequest struct {
	RepositoryName string            `json:"repositoryName"`
	RegistryID     string            `json:"registryId"`
	ImageIDs       []imageIdentifier `json:"imageIds"`
}

// BatchDeleteImage removes tag pointers or manifests by digest. A tag removes
// only that pointer; a digest removes its manifest and all tags resolving to
// it. Per ECR's batch contract, individual misses are structured failures in a
// successful response rather than a failed request.
func BatchDeleteImage(ctx context.Context, deleter ImageDeleter, accountID string, body []byte) (*ecr.BatchDeleteImageOutput, error) {
	if deleter == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var req batchDeleteImageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return nil, err
	}
	if len(req.ImageIDs) > maxImageBatch {
		return nil, MaxItemsError("imageIds", maxImageBatch)
	}

	deleted := make([]*ecr.ImageIdentifier, 0, len(req.ImageIDs))
	failures := make([]*ecr.ImageFailure, 0)
	for _, imageID := range req.ImageIDs {
		if imageID.ImageDigest == "" && imageID.ImageTag == "" {
			failures = append(failures, imageFailure(imageID, ecr.ImageFailureCodeMissingDigestAndTag, "no digest or tag specified"))
			continue
		}

		digest, err := deleter.DeleteImage(ctx, accountID, req.RepositoryName, imageID.ImageTag, imageID.ImageDigest)
		if errors.Is(err, ecrregistry.ErrImageNotFound) {
			failures = append(failures, imageFailure(imageID, ecr.ImageFailureCodeImageNotFound, ImageNotFoundReason))
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "ECR BatchDeleteImage: delete image failed", "repository", req.RepositoryName, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		deletedImage := &ecr.ImageIdentifier{ImageDigest: aws.String(digest)}
		if imageID.ImageTag != "" {
			deletedImage.ImageTag = aws.String(imageID.ImageTag)
		}
		deleted = append(deleted, deletedImage)
	}

	return &ecr.BatchDeleteImageOutput{ImageIds: deleted, Failures: failures}, nil
}
