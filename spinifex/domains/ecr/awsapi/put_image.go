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

type putImageRequest struct {
	RepositoryName         string `json:"repositoryName"`
	RegistryID             string `json:"registryId"`
	ImageManifest          string `json:"imageManifest"`
	ImageManifestMediaType string `json:"imageManifestMediaType"`
	ImageTag               string `json:"imageTag"`
	ImageDigest            string `json:"imageDigest"`
}

// PutImage stores a control-plane-supplied manifest and optional tag. It is
// the AWS JSON twin of OCI Distribution manifest PUT; the registry owns
// manifest validation and storage, while this action maps its result to the
// ECR AWS API contract.
func PutImage(ctx context.Context, writer ManifestWriter, accountID string, body []byte) (*ecr.PutImageOutput, error) {
	if writer == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var req putImageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return nil, err
	}
	if req.ImageManifest == "" {
		return nil, RequiredParameterError("imageManifest")
	}

	reference := req.ImageTag
	if reference == "" {
		reference = req.ImageDigest
	}
	digest, err := writer.StoreManifest(ctx, accountID, req.RepositoryName, reference, req.ImageManifestMediaType, []byte(req.ImageManifest))
	if err != nil {
		return nil, mapStoreManifestError(ctx, err, req.RepositoryName)
	}

	image := &ecr.Image{
		RegistryId:             aws.String(accountID),
		RepositoryName:         aws.String(req.RepositoryName),
		ImageId:                &ecr.ImageIdentifier{ImageDigest: aws.String(digest)},
		ImageManifest:          aws.String(req.ImageManifest),
		ImageManifestMediaType: aws.String(req.ImageManifestMediaType),
	}
	if req.ImageTag != "" {
		image.ImageId.ImageTag = aws.String(req.ImageTag)
	}
	return &ecr.PutImageOutput{Image: image}, nil
}

// mapStoreManifestError translates OCI manifest-store errors into AWS PutImage
// errors. The OCI adapter's typed errors are part of the explicit ECR internal
// capability boundary; unknown backend faults remain ServerInternal.
func mapStoreManifestError(ctx context.Context, err error, repository string) error {
	if manifestErr, ok := errors.AsType[*ecrregistry.ManifestStoreError](err); ok {
		switch manifestErr.Code {
		case "DIGEST_INVALID":
			return errors.New(awserrors.ErrorImageDigestDoesNotMatch)
		case "MANIFEST_BLOB_UNKNOWN":
			return errors.New(awserrors.ErrorLayersNotFound)
		case "TAG_IMMUTABLE":
			return errors.New(awserrors.ErrorImageTagAlreadyExists)
		case "NAME_UNKNOWN":
			return errors.New(awserrors.ErrorRepositoryNotFound)
		default:
			return ConstraintError("imageManifest", manifestErr.Msg)
		}
	}
	slog.ErrorContext(ctx, "ECR PutImage: store manifest failed", "repository", repository, "err", err)
	return errors.New(awserrors.ErrorServerInternal)
}
