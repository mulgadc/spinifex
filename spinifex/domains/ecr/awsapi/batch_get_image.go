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

// maxImageBatch is the AWS per-call cap on imageIds for batch image actions.
const maxImageBatch = 100

type batchGetImageRequest struct {
	RepositoryName     string            `json:"repositoryName"`
	RegistryID         string            `json:"registryId"`
	ImageIDs           []imageIdentifier `json:"imageIds"`
	AcceptedMediaTypes []string          `json:"acceptedMediaTypes"`
}

// BatchGetImage returns manifest bytes for up to 100 image identifiers. Per
// Q14, individual misses return a successful response containing structured
// failures; a supplied digest takes precedence over an image tag.
func BatchGetImage(ctx context.Context, reader ManifestReader, accountID string, body []byte) (*ecr.BatchGetImageOutput, error) {
	if reader == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var req batchGetImageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return nil, err
	}
	if len(req.ImageIDs) > maxImageBatch {
		return nil, MaxItemsError("imageIds", maxImageBatch)
	}

	images := make([]*ecr.Image, 0, len(req.ImageIDs))
	failures := make([]*ecr.ImageFailure, 0)
	for _, imageID := range req.ImageIDs {
		reference := imageID.ImageDigest
		if reference == "" {
			reference = imageID.ImageTag
		}
		if reference == "" {
			failures = append(failures, imageFailure(imageID, ecr.ImageFailureCodeMissingDigestAndTag, "no digest or tag specified"))
			continue
		}

		manifest, mediaType, digest, err := reader.GetManifest(ctx, accountID, req.RepositoryName, reference, req.AcceptedMediaTypes)
		if errors.Is(err, ecrregistry.ErrImageNotFound) {
			failures = append(failures, imageFailure(imageID, ecr.ImageFailureCodeImageNotFound, ImageNotFoundReason))
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "ECR BatchGetImage: get manifest failed", "repository", req.RepositoryName, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		image := &ecr.Image{
			RegistryId:             aws.String(accountID),
			RepositoryName:         aws.String(req.RepositoryName),
			ImageId:                &ecr.ImageIdentifier{ImageDigest: aws.String(digest)},
			ImageManifest:          aws.String(string(manifest)),
			ImageManifestMediaType: aws.String(mediaType),
		}
		if imageID.ImageTag != "" {
			image.ImageId.ImageTag = aws.String(imageID.ImageTag)
		}
		images = append(images, image)
	}

	return &ecr.BatchGetImageOutput{Images: images, Failures: failures}, nil
}

func imageFailure(imageID imageIdentifier, code, reason string) *ecr.ImageFailure {
	failure := &ecr.ImageFailure{
		FailureCode:   aws.String(code),
		FailureReason: aws.String(reason),
		ImageId:       &ecr.ImageIdentifier{},
	}
	if imageID.ImageDigest != "" {
		failure.ImageId.ImageDigest = aws.String(imageID.ImageDigest)
	}
	if imageID.ImageTag != "" {
		failure.ImageId.ImageTag = aws.String(imageID.ImageTag)
	}
	return failure
}
