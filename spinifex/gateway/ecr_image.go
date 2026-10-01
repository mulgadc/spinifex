package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// maxImageBatch is the per-call cap on imageIds for the batch image actions.
const maxImageBatch = 100

// imageIdentifier is the AWS JSON 1.1 {imageDigest, imageTag} pair used by
// the remaining gateway image-action adapters. It will leave with those
// actions; DescribeImages has its domain-local input shape already.
type imageIdentifier struct {
	ImageDigest string `json:"imageDigest"`
	ImageTag    string `json:"imageTag"`
}

type batchDeleteImageRequest struct {
	RepositoryName string            `json:"repositoryName"`
	RegistryID     string            `json:"registryId"`
	ImageIds       []imageIdentifier `json:"imageIds"`
}

// ecrImageAccount reads the auth-context account and requires the OCI registry
// to be wired. Image actions need the gateway-side predastore Store, so a nil
// registry is a server fault.
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

// validateRepoAndRegistry rejects a malformed repository name and a registryId
// targeting another account.
func validateRepoAndRegistry(name, registryID, accountID string) error {
	if err := awsapi.ValidateRepositoryName(name); err != nil {
		return err
	}
	if registryID != "" && registryID != accountID {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

func decodeJSONBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return awsapi.MalformedBodyError()
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return awsapi.MalformedBodyError()
	}
	return nil
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

// handleBatchDeleteImage deletes images by tag or digest. A tag removes only
// that tag pointer; a digest removes the manifest and every tag pointing at it.
// Partial failures are reported in the failures array with HTTP 200.
func (gw *GatewayConfig) handleBatchDeleteImage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	var req batchDeleteImageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return err
	}
	if err := validateRepoAndRegistry(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return err
	}
	if len(req.ImageIds) > maxImageBatch {
		return awsapi.MaxItemsError("imageIds", maxImageBatch)
	}

	var deleted []*ecr.ImageIdentifier
	var failures []*ecr.ImageFailure
	for _, id := range req.ImageIds {
		if id.ImageDigest == "" && id.ImageTag == "" {
			failures = append(failures, imageFailure(id, ecr.ImageFailureCodeMissingDigestAndTag, "no digest or tag specified"))
			continue
		}

		digest, dErr := gw.ECRRegistry.DeleteImage(ctx, accountID, req.RepositoryName, id.ImageTag, id.ImageDigest)
		if errors.Is(dErr, ecrregistry.ErrImageNotFound) {
			failures = append(failures, imageFailure(id, ecr.ImageFailureCodeImageNotFound, "image not found"))
			continue
		}
		if dErr != nil {
			slog.ErrorContext(ctx, "BatchDeleteImage: delete failed", "repo", req.RepositoryName, "err", dErr)
			return errors.New(awserrors.ErrorServerInternal)
		}

		out := &ecr.ImageIdentifier{ImageDigest: aws.String(digest)}
		if id.ImageTag != "" {
			out.ImageTag = aws.String(id.ImageTag)
		}
		deleted = append(deleted, out)
	}

	awsapi.WriteJSONResponse(w, &ecr.BatchDeleteImageOutput{ImageIds: deleted, Failures: failures})
	return nil
}

func imageFailure(id imageIdentifier, code, reason string) *ecr.ImageFailure {
	failure := &ecr.ImageFailure{
		FailureCode:   aws.String(code),
		FailureReason: aws.String(reason),
		ImageId:       &ecr.ImageIdentifier{},
	}
	if id.ImageDigest != "" {
		failure.ImageId.ImageDigest = aws.String(id.ImageDigest)
	}
	if id.ImageTag != "" {
		failure.ImageId.ImageTag = aws.String(id.ImageTag)
	}
	return failure
}
