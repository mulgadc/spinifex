package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
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
	Expiries            []ecrdomain.LifecycleExpiry
}

// lifecyclePreviewRequest is the camelCase AWS JSON 1.1 input shared by the
// two preview actions.
type lifecyclePreviewRequest struct {
	RepositoryName      string `json:"repositoryName"`
	RegistryID          string `json:"registryId"`
	LifecyclePolicyText string `json:"lifecyclePolicyText"`
}

// getLifecyclePreviewRequest is the GetLifecyclePolicyPreview-only input that
// narrows and pages the preview results.
type getLifecyclePreviewRequest struct {
	ImageIDs   []imageIdentifier `json:"imageIds"`
	NextToken  *string           `json:"nextToken"`
	MaxResults *int64            `json:"maxResults"`
	Filter     *struct {
		TagStatus string `json:"tagStatus"`
	} `json:"filter"`
}

// LifecyclePreviewMaxResults' range in the ECR model.
const (
	minLifecyclePreviewResults = 1
	maxLifecyclePreviewResults = 100
)

// parse decodes and validates the paging and filter members of body.
func (r *getLifecyclePreviewRequest) parse(body []byte) (paging.Request, error) {
	if err := json.Unmarshal(body, r); err != nil {
		return paging.Request{}, MalformedBodyError()
	}
	if err := validateImageIDs(r.ImageIDs, false); err != nil {
		return paging.Request{}, err
	}
	limit := int64(maxLifecyclePreviewResults)
	if r.MaxResults != nil {
		limit = *r.MaxResults
	}
	switch {
	case limit < minLifecyclePreviewResults:
		return paging.Request{}, ConstraintError("maxResults", fmt.Sprintf("Member must have value greater than or equal to %d", minLifecyclePreviewResults))
	case limit > maxLifecyclePreviewResults:
		return paging.Request{}, ConstraintError("maxResults", fmt.Sprintf("Member must have value less than or equal to %d", maxLifecyclePreviewResults))
	}
	if r.Filter != nil && r.Filter.TagStatus != "" && !slices.Contains(TagStatusValues, r.Filter.TagStatus) {
		return paging.Request{}, EnumValueError("filter.tagStatus", TagStatusValues...)
	}
	from, err := paging.DecodeToken(aws.StringValue(r.NextToken))
	if err != nil {
		return paging.Request{}, awserrors.Errorf(awserrors.ErrorECRInvalidParameter, "Invalid parameter at 'nextToken': the token is not valid")
	}
	return paging.Request{Limit: int(limit), From: from}, nil
}

// wants reports whether expiry passes the imageIds and filter.tagStatus members.
func (r *getLifecyclePreviewRequest) wants(expiry ecrdomain.LifecycleExpiry) bool {
	if r.Filter != nil {
		switch r.Filter.TagStatus {
		case ecr.TagStatusTagged:
			if len(expiry.Tags) == 0 {
				return false
			}
		case ecr.TagStatusUntagged:
			if len(expiry.Tags) > 0 {
				return false
			}
		}
	}
	if len(r.ImageIDs) == 0 {
		return true
	}
	for _, image := range r.ImageIDs {
		if image.ImageDigest != "" && image.ImageDigest == expiry.Digest {
			return true
		}
		if image.ImageTag != nil && slices.Contains(expiry.Tags, *image.ImageTag) {
			return true
		}
	}
	return false
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
	if policyText != "" {
		if err := ValidateTextLength("lifecyclePolicyText", policyText, minLifecyclePolicyTextLength, maxLifecyclePolicyTextLength); err != nil {
			return LifecyclePreview{}, err
		}
	} else {
		stored, err := policies.GetLifecyclePolicy(ctx, accountID, req.RepositoryName)
		if err != nil {
			if errors.Is(err, ecrdomain.ErrNotFound) {
				return LifecyclePreview{}, LifecyclePolicyNotFoundError(accountID, req.RepositoryName)
			}
			return LifecyclePreview{}, err
		}
		policyText = string(stored)
	}

	records, err := listImageRecords(ctx, catalog, accountID, req.RepositoryName)
	if err != nil {
		return LifecyclePreview{}, err
	}
	images := make([]ecrdomain.LifecycleImage, 0, len(records))
	for _, record := range records {
		images = append(images, ecrdomain.LifecycleImage{
			Digest: record.Digest, Tags: record.Tags, PushedAt: record.PushedAt,
		})
	}

	expiries, err := ecrdomain.EvaluateLifecyclePolicy([]byte(policyText), images, time.Now().UTC())
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

// GetLifecyclePolicyPreview evaluates synchronously and returns one page of
// the expiry results that match imageIds and filter, ordered by digest. The
// summary counts every expiring image. The status is always COMPLETE.
func GetLifecyclePolicyPreview(ctx context.Context, policies LifecyclePolicyStore, catalog ImageCatalog, accountID string, body []byte) (*ecr.GetLifecyclePolicyPreviewOutput, error) {
	var req getLifecyclePreviewRequest
	pageReq, err := req.parse(body)
	if err != nil {
		return nil, err
	}
	preview, err := EvaluateLifecyclePreview(ctx, policies, catalog, accountID, body)
	if err != nil {
		return nil, err
	}

	matched := make([]ecrdomain.LifecycleExpiry, 0, len(preview.Expiries))
	for _, expiry := range preview.Expiries {
		if req.wants(expiry) {
			matched = append(matched, expiry)
		}
	}
	page, next := paging.Page(matched, func(e ecrdomain.LifecycleExpiry) string { return e.Digest }, pageReq)

	results := make([]*ecr.LifecyclePolicyPreviewResult, 0, len(page))
	for _, expiry := range page {
		results = append(results, &ecr.LifecyclePolicyPreviewResult{
			Action:              &ecr.LifecyclePolicyRuleAction{Type: aws.String(ecr.ImageActionTypeExpire)},
			AppliedRulePriority: aws.Int64(int64(expiry.RulePriority)),
			ImageDigest:         aws.String(expiry.Digest),
			ImagePushedAt:       aws.Time(expiry.PushedAt),
			ImageTags:           aws.StringSlice(expiry.Tags),
		})
	}
	out := &ecr.GetLifecyclePolicyPreviewOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(preview.RepositoryName),
		LifecyclePolicyText: aws.String(preview.LifecyclePolicyText),
		Status:              aws.String(ecr.LifecyclePolicyPreviewStatusComplete),
		PreviewResults:      results,
		Summary:             &ecr.LifecyclePolicyPreviewSummary{ExpiringImageTotalCount: aws.Int64(int64(len(preview.Expiries)))},
	}
	if next != "" {
		out.NextToken = aws.String(next)
	}
	return out, nil
}
