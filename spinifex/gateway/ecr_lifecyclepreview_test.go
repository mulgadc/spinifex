package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expireAllPolicy expires every image (imageCountMoreThan 0 is invalid, so use a
// sinceImagePushed window of 0 days against any tag: images pushed before now).
const previewExpireOldest = `{"rules":[{"rulePriority":7,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`

func previewBody(repo, policy string) string {
	b, _ := json.Marshal(map[string]string{"repositoryName": repo, "lifecyclePolicyText": policy})
	return string(b)
}

func TestStartLifecyclePolicyPreview_Override(t *testing.T) {
	gw := newImageGateway(t)
	seedTaggedImage(t, gw, "team/app", "v1")
	seedTaggedImage(t, gw, "team/app", "v2")

	w, err := callImage(t, gw, (*GatewayConfig).handleStartLifecyclePolicyPreview, previewBody("team/app", previewExpireOldest))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	var out ecr.StartLifecyclePolicyPreviewOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, ecr.LifecyclePolicyPreviewStatusComplete, *out.Status)
	assert.Equal(t, previewExpireOldest, *out.LifecyclePolicyText)
}

func TestGetLifecyclePolicyPreview_Override(t *testing.T) {
	gw := newImageGateway(t)
	seedTaggedImage(t, gw, "team/app", "v1")
	seedTaggedImage(t, gw, "team/app", "v2")
	seedTaggedImage(t, gw, "team/app", "v3")

	w, err := callImage(t, gw, (*GatewayConfig).handleGetLifecyclePolicyPreview, previewBody("team/app", previewExpireOldest))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)

	// jsonutil emits imagePushedAt as epoch float; decode via a local view.
	var out struct {
		Status         string `json:"status"`
		PreviewResults []struct {
			ImageDigest         string   `json:"imageDigest"`
			ImageTags           []string `json:"imageTags"`
			AppliedRulePriority int64    `json:"appliedRulePriority"`
			Action              struct {
				Type string `json:"type"`
			} `json:"action"`
		} `json:"previewResults"`
		Summary struct {
			ExpiringImageTotalCount int64 `json:"expiringImageTotalCount"`
		} `json:"lifecyclePolicyRuleSummary"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "COMPLETE", out.Status)
	// imageCountMoreThan 1 over 3 images expires the 2 oldest.
	require.Len(t, out.PreviewResults, 2)
	for _, r := range out.PreviewResults {
		assert.Equal(t, "EXPIRE", r.Action.Type)
		assert.Equal(t, int64(7), r.AppliedRulePriority)
	}
}

func TestLifecyclePreview_Errors(t *testing.T) {
	gw := newImageGateway(t)
	seedTaggedImage(t, gw, "team/app", "v1")

	// Missing repo.
	_, err := callImage(t, gw, (*GatewayConfig).handleGetLifecyclePolicyPreview, previewBody("team/ghost", previewExpireOldest))
	require.Error(t, err)
	assert.Equal(t, "RepositoryNotFoundException", err.Error())

	// Malformed override policy.
	_, err = callImage(t, gw, (*GatewayConfig).handleGetLifecyclePolicyPreview, previewBody("team/app", "not-json"))
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())

	// Cross-account.
	_, err = callImage(t, gw, (*GatewayConfig).handleStartLifecyclePolicyPreview, `{"repositoryName":"team/app","registryId":"999999999999","lifecyclePolicyText":`+strconvQuotePreview(previewExpireOldest)+`}`)
	require.Error(t, err)
	assert.Equal(t, "AccessDenied", err.Error())
}

func strconvQuotePreview(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
