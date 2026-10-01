package awsapi

import (
	"context"
	"errors"
	"log/slog"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ImageCatalog is the read-only ECR registry capability image-listing actions
// need. The OCI Distribution adapter owns the concrete registry and storage
// wiring; the AWS adapter owns the ECR JSON request and response shapes.
type ImageCatalog interface {
	ListImages(ctx context.Context, account, repository string) ([]ecrregistry.ImageRecord, error)
}

func listImageRecords(ctx context.Context, catalog ImageCatalog, accountID, repository string) ([]ecrregistry.ImageRecord, error) {
	if catalog == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	records, err := catalog.ListImages(ctx, accountID, repository)
	if errors.Is(err, handlers_ecr.ErrNotFound) {
		return nil, errors.New(awserrors.ErrorRepositoryNotFound)
	}
	if err != nil {
		slog.ErrorContext(ctx, "ECR image action: list images failed", "repository", repository, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return records, nil
}
