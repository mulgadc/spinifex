package gateway

import (
	"io"
	"net/http"

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

// handleGetLifecyclePolicyPreview adapts the authenticated HTTP request to the
// ECR action. Lifecycle evaluation and AWS response projection live in the
// ECR adapter; gateway supplies the policy store and image catalog.
func (gw *GatewayConfig) handleGetLifecyclePolicyPreview(w http.ResponseWriter, r *http.Request) error {
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.GetLifecyclePolicyPreview(r.Context(), handlers_ecr.NewNATSMetaStore(gw.NATSConn), gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}
