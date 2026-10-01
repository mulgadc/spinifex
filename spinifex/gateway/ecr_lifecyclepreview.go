package gateway

import (
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
)

// handleStartLifecyclePolicyPreview adapts the authenticated HTTP request to
// the ECR action. Lifecycle evaluation and AWS response semantics live in the
// ECR adapter; gateway supplies the policy store and image catalog.
func (gw *GatewayConfig) handleStartLifecyclePolicyPreview(w http.ResponseWriter, r *http.Request) error {
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.StartLifecyclePolicyPreview(r.Context(), handlers_ecr.NewNATSMetaStore(gw.NATSConn), gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}

// handleGetLifecyclePolicyPreview returns the evaluated expiry set.
func (gw *GatewayConfig) handleGetLifecyclePolicyPreview(w http.ResponseWriter, r *http.Request) error {
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	preview, err := awsapi.EvaluateLifecyclePreview(r.Context(), handlers_ecr.NewNATSMetaStore(gw.NATSConn), gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}

	results := make([]*ecr.LifecyclePolicyPreviewResult, 0, len(preview.Expiries))
	for _, e := range preview.Expiries {
		results = append(results, &ecr.LifecyclePolicyPreviewResult{
			Action:              &ecr.LifecyclePolicyRuleAction{Type: aws.String(ecr.ImageActionTypeExpire)},
			AppliedRulePriority: aws.Int64(int64(e.RulePriority)),
			ImageDigest:         aws.String(e.Digest),
			ImagePushedAt:       aws.Time(e.PushedAt),
			ImageTags:           aws.StringSlice(e.Tags),
		})
	}
	awsapi.WriteJSONResponse(w, &ecr.GetLifecyclePolicyPreviewOutput{
		RegistryId:          aws.String(accountID),
		RepositoryName:      aws.String(preview.RepositoryName),
		LifecyclePolicyText: aws.String(preview.LifecyclePolicyText),
		Status:              aws.String(ecr.LifecyclePolicyPreviewStatusComplete),
		PreviewResults:      results,
		Summary:             &ecr.LifecyclePolicyPreviewSummary{ExpiringImageTotalCount: aws.Int64(int64(len(preview.Expiries)))},
	})
	return nil
}
