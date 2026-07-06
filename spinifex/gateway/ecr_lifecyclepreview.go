package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// lifecyclePreviewRequest is the camelCase AWS JSON 1.1 input shared by the two
// preview actions. Pagination (maxResults/nextToken), imageIds and filter are
// accepted-and-ignored in v1; the full result set is returned in one page.
type lifecyclePreviewRequest struct {
	RepositoryName      string `json:"repositoryName"`
	RegistryID          string `json:"registryId"`
	LifecyclePolicyText string `json:"lifecyclePolicyText"`
}

// evaluateLifecyclePreview resolves the policy text (request override or stored)
// and evaluates it against the repository's current images. A missing policy
// (no override and none stored) yields LifecyclePolicyNotFoundException.
func (gw *GatewayConfig) evaluateLifecyclePreview(r *http.Request) (string, []handlers_ecr.LifecycleExpiry, *lifecyclePreviewRequest, error) {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return "", nil, nil, err
	}
	var req lifecyclePreviewRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return "", nil, nil, err
	}
	if err := validateRepoAndRegistry(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return "", nil, nil, err
	}

	policyText := req.LifecyclePolicyText
	if policyText == "" {
		store := handlers_ecr.NewNATSMetaStore(gw.NATSConn)
		stored, err := store.GetLifecyclePolicy(ctx, accountID, req.RepositoryName)
		if err != nil {
			if errors.Is(err, handlers_ecr.ErrNotFound) {
				return "", nil, nil, errors.New(awserrors.ErrorLifecyclePolicyNotFound)
			}
			return "", nil, nil, err
		}
		policyText = string(stored)
	}

	records, err := gw.ECRRegistry.ListImages(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return "", nil, nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		return "", nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	images := make([]handlers_ecr.LifecycleImage, 0, len(records))
	for _, rec := range records {
		images = append(images, handlers_ecr.LifecycleImage{Digest: rec.Digest, Tags: rec.Tags, PushedAt: rec.PushedAt})
	}

	expiries, err := handlers_ecr.EvaluateLifecyclePolicy([]byte(policyText), images, time.Now().UTC())
	if err != nil {
		return "", nil, nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return policyText, expiries, &req, nil
}

// handleStartLifecyclePolicyPreview evaluates the policy synchronously and
// reports COMPLETE; v1 has no asynchronous preview job.
func (gw *GatewayConfig) handleStartLifecyclePolicyPreview(w http.ResponseWriter, r *http.Request) error {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	policyText, _, req, err := gw.evaluateLifecyclePreview(r)
	if err != nil {
		return err
	}
	gateway_ecrapi.WriteJSONResponse(w, &ecr.StartLifecyclePolicyPreviewOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(policyText),
		Status:              aws.String(ecr.LifecyclePolicyPreviewStatusComplete),
	})
	return nil
}

// handleGetLifecyclePolicyPreview returns the evaluated expiry set.
func (gw *GatewayConfig) handleGetLifecyclePolicyPreview(w http.ResponseWriter, r *http.Request) error {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	policyText, expiries, req, err := gw.evaluateLifecyclePreview(r)
	if err != nil {
		return err
	}

	results := make([]*ecr.LifecyclePolicyPreviewResult, 0, len(expiries))
	for _, e := range expiries {
		results = append(results, &ecr.LifecyclePolicyPreviewResult{
			Action:              &ecr.LifecyclePolicyRuleAction{Type: aws.String(ecr.ImageActionTypeExpire)},
			AppliedRulePriority: aws.Int64(int64(e.RulePriority)),
			ImageDigest:         aws.String(e.Digest),
			ImagePushedAt:       aws.Time(e.PushedAt),
			ImageTags:           aws.StringSlice(e.Tags),
		})
	}
	gateway_ecrapi.WriteJSONResponse(w, &ecr.GetLifecyclePolicyPreviewOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(req.RepositoryName),
		LifecyclePolicyText: aws.String(policyText),
		Status:              aws.String(ecr.LifecyclePolicyPreviewStatusComplete),
		PreviewResults:      results,
		Summary:             &ecr.LifecyclePolicyPreviewSummary{ExpiringImageTotalCount: aws.Int64(int64(len(expiries)))},
	})
	return nil
}
