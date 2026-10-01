package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// LifecyclePolicyStore is the account-scoped policy lookup capability a
// preview uses when the caller does not supply lifecyclePolicyText directly.
type LifecyclePolicyStore interface {
	GetLifecyclePolicy(ctx context.Context, accountID, repository string) ([]byte, error)
}

// LifecyclePreview is the domain result shared by the start and get preview
// actions. v1 evaluates synchronously: it has no durable preview-job state.
type LifecyclePreview struct {
	RepositoryName      string
	LifecyclePolicyText string
	Expiries            []handlers_ecr.LifecycleExpiry
}

// lifecyclePreviewRequest is the camelCase AWS JSON 1.1 input shared by the
// two preview actions. Pagination, imageIds and filter remain accepted and
// ignored in v1; the full evaluated set is returned in one page.
type lifecyclePreviewRequest struct {
	RepositoryName      string `json:"repositoryName"`
	RegistryID          string `json:"registryId"`
	LifecyclePolicyText string `json:"lifecyclePolicyText"`
}

// EvaluateLifecyclePreview resolves request or stored policy text and applies
// it to the repository's current image metadata. A missing stored policy is an
// ECR LifecyclePolicyNotFoundException; an invalid policy is an AWS invalid
// parameter response.
func EvaluateLifecyclePreview(ctx context.Context, policies LifecyclePolicyStore, catalog ImageCatalog, accountID string, body []byte) (LifecyclePreview, error) {
	if policies == nil || catalog == nil {
		return LifecyclePreview{}, errors.New(awserrors.ErrorServerInternal)
	}
	var req lifecyclePreviewRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return LifecyclePreview{}, MalformedBodyError()
	}
	if err := ValidateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return LifecyclePreview{}, err
	}

	policyText := req.LifecyclePolicyText
	if policyText == "" {
		stored, err := policies.GetLifecyclePolicy(ctx, accountID, req.RepositoryName)
		if err != nil {
			if errors.Is(err, handlers_ecr.ErrNotFound) {
				return LifecyclePreview{}, errors.New(awserrors.ErrorLifecyclePolicyNotFound)
			}
			return LifecyclePreview{}, err
		}
		policyText = string(stored)
	}

	records, err := listImageRecords(ctx, catalog, accountID, req.RepositoryName)
	if err != nil {
		return LifecyclePreview{}, err
	}
	images := make([]handlers_ecr.LifecycleImage, 0, len(records))
	for _, record := range records {
		images = append(images, handlers_ecr.LifecycleImage{
			Digest: record.Digest, Tags: record.Tags, PushedAt: record.PushedAt,
		})
	}

	expiries, err := handlers_ecr.EvaluateLifecyclePolicy([]byte(policyText), images, time.Now().UTC())
	if err != nil {
		return LifecyclePreview{}, InvalidLifecyclePolicyError()
	}
	return LifecyclePreview{
		RepositoryName: req.RepositoryName, LifecyclePolicyText: policyText, Expiries: expiries,
	}, nil
}

// StartLifecyclePolicyPreview evaluates synchronously and reports COMPLETE.
// v1 does not create an asynchronous preview job.
func StartLifecyclePolicyPreview(ctx context.Context, policies LifecyclePolicyStore, catalog ImageCatalog, accountID string, body []byte) (*ecr.StartLifecyclePolicyPreviewOutput, error) {
	preview, err := EvaluateLifecyclePreview(ctx, policies, catalog, accountID, body)
	if err != nil {
		return nil, err
	}
	return &ecr.StartLifecyclePolicyPreviewOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(preview.RepositoryName),
		LifecyclePolicyText: aws.String(preview.LifecyclePolicyText),
		Status:              aws.String(ecr.LifecyclePolicyPreviewStatusComplete),
	}, nil
}
