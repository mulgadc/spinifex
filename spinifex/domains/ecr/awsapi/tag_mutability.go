package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// putImageTagMutabilityRequest is the camelCase AWS JSON 1.1 input shape.
type putImageTagMutabilityRequest struct {
	RepositoryName     string `json:"repositoryName"`
	RegistryID         string `json:"registryId"`
	ImageTagMutability string `json:"imageTagMutability"`
}

// PutImageTagMutability changes a repository between MUTABLE and IMMUTABLE.
// Unlike CreateRepository, the mutability field is required. The persisted
// value is enforced by the ECR registry when it stores a manifest.
func PutImageTagMutability(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	var req putImageTagMutabilityRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryName(req.RepositoryName); err != nil {
		return nil, err
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	switch req.ImageTagMutability {
	case handlers_ecr.TagMutabilityMutable, handlers_ecr.TagMutabilityImmutable:
	case "":
		return nil, RequiredParameterError("imageTagMutability")
	default:
		return nil, EnumValueError("imageTagMutability", req.ImageTagMutability,
			handlers_ecr.TagMutabilityMutable, handlers_ecr.TagMutabilityImmutable)
	}

	store := handlers_ecr.NewNATSMetaStore(nc)
	meta, err := store.GetRepo(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		slog.ErrorContext(ctx, "ECR PutImageTagMutability: get repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	meta.ImageTagMutability = req.ImageTagMutability
	if err := store.PutRepo(ctx, accountID, meta); err != nil {
		slog.ErrorContext(ctx, "ECR PutImageTagMutability: put repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &ecr.PutImageTagMutabilityOutput{
		RegistryId:         aws.String(accountID),
		RepositoryName:     aws.String(req.RepositoryName),
		ImageTagMutability: aws.String(meta.TagMutability()),
	}, nil
}
