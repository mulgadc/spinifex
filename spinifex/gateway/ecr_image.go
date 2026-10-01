package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ecrImageAccount reads the auth-context account and requires the composed OCI
// registry capability. A missing registry is a deployment/composition fault.
func (gw *GatewayConfig) ecrImageAccount(r *http.Request) (string, error) {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ECR image action: no account ID in auth context")
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	if gw.ECRRegistry == nil {
		slog.ErrorContext(r.Context(), "ECR image action: OCI registry not configured")
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	return accountID, nil
}

// handleListImages adapts the authenticated HTTP request to the ECR ListImages
// action. The ECR AWS adapter owns request and response semantics; gateway
// supplies the configured OCI registry capability.
func (gw *GatewayConfig) handleListImages(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.ListImages(ctx, gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}

// handleDescribeImages adapts the authenticated HTTP request to the ECR
// DescribeImages action. The ECR AWS adapter owns request and response
// semantics; gateway supplies the configured OCI registry capability.
func (gw *GatewayConfig) handleDescribeImages(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.DescribeImages(ctx, gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}

// handleBatchGetImage adapts the authenticated HTTP request to the ECR AWS
// action. The ECR adapter owns request semantics and partial-failure response
// construction; gateway supplies the configured OCI registry capability.
func (gw *GatewayConfig) handleBatchGetImage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.BatchGetImage(ctx, gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}

// handlePutImage adapts the authenticated HTTP request to the ECR PutImage
// action. The ECR adapter owns request semantics and OCI-to-AWS result mapping;
// gateway supplies the configured OCI registry capability.
func (gw *GatewayConfig) handlePutImage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.PutImage(ctx, gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}

// handleBatchDeleteImage adapts the authenticated HTTP request to the ECR AWS
// action. The ECR adapter owns batch partial-failure semantics; gateway
// supplies the configured OCI registry capability.
func (gw *GatewayConfig) handleBatchDeleteImage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.BatchDeleteImage(ctx, gw.ECRRegistry, accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}
