package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ImageCatalog is the read-only ECR registry capability ListImages needs. The
// OCI Distribution adapter owns the concrete registry and storage wiring; the
// AWS adapter owns the ECR JSON request and response shapes.
type ImageCatalog interface {
	ListImages(ctx context.Context, account, repository string) ([]ecrregistry.ImageRecord, error)
}

type listImagesRequest struct {
	RepositoryName string           `json:"repositoryName"`
	RegistryID     string           `json:"registryId"`
	Filter         *tagStatusFilter `json:"filter"`
}

type tagStatusFilter struct {
	TagStatus string `json:"tagStatus"`
}

// ListImages returns every image identifier in the caller's repository,
// optionally filtering tagged or untagged manifests. Each tag is an AWS image
// identifier; an untagged manifest is represented by its digest only.
func ListImages(ctx context.Context, catalog ImageCatalog, accountID string, body []byte) (*ecr.ListImagesOutput, error) {
	if catalog == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var req listImagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryName(req.RepositoryName); err != nil {
		return nil, err
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}

	records, err := catalog.ListImages(ctx, accountID, req.RepositoryName)
	if errors.Is(err, handlers_ecr.ErrNotFound) {
		return nil, errors.New(awserrors.ErrorRepositoryNotFound)
	}
	if err != nil {
		slog.ErrorContext(ctx, "ECR ListImages: list images failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	status := ""
	if req.Filter != nil {
		status = req.Filter.TagStatus
	}
	identifiers := make([]*ecr.ImageIdentifier, 0, len(records))
	for _, record := range records {
		if len(record.Tags) == 0 {
			if status == ecr.TagStatusTagged {
				continue
			}
			identifiers = append(identifiers, &ecr.ImageIdentifier{ImageDigest: aws.String(record.Digest)})
			continue
		}
		if status == ecr.TagStatusUntagged {
			continue
		}
		for _, tag := range record.Tags {
			identifiers = append(identifiers, &ecr.ImageIdentifier{
				ImageDigest: aws.String(record.Digest),
				ImageTag:    aws.String(tag),
			})
		}
	}

	return &ecr.ListImagesOutput{ImageIds: identifiers}, nil
}
