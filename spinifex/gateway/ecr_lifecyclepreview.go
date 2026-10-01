package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ecrLifecyclePreviewAccount obtains the authenticated account and the
// registry capability still required by the transitional lifecycle-preview
// adapters. Registry-backed image actions use RegistryActionService instead.
func (gw *GatewayConfig) ecrLifecyclePreviewAccount(r *http.Request) (string, error) {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ECR lifecycle preview: no account ID in auth context")
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	if gw.ECRRegistry == nil {
		slog.ErrorContext(r.Context(), "ECR lifecycle preview: OCI registry not configured")
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	return accountID, nil
}

// handleStartLifecyclePolicyPreview adapts the authenticated HTTP request to
// the ECR action. Lifecycle evaluation and AWS response semantics live in the
// ECR adapter; gateway supplies the policy store and image catalog.
func (gw *GatewayConfig) handleStartLifecyclePolicyPreview(w http.ResponseWriter, r *http.Request) error {
	accountID, err := gw.ecrLifecyclePreviewAccount(r)
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
	accountID, err := gw.ecrLifecyclePreviewAccount(r)
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
