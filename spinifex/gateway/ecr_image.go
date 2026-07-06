package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// maxImageBatch is the per-call cap on imageIds for the batch image actions.
const maxImageBatch = 100

// imageIdentifier is the AWS JSON 1.1 {imageDigest, imageTag} pair.
type imageIdentifier struct {
	ImageDigest string `json:"imageDigest"`
	ImageTag    string `json:"imageTag"`
}

type tagStatusFilter struct {
	TagStatus string `json:"tagStatus"`
}

type listImagesRequest struct {
	RepositoryName string           `json:"repositoryName"`
	RegistryID     string           `json:"registryId"`
	Filter         *tagStatusFilter `json:"filter"`
}

type describeImagesRequest struct {
	RepositoryName string            `json:"repositoryName"`
	RegistryID     string            `json:"registryId"`
	ImageIds       []imageIdentifier `json:"imageIds"`
	Filter         *tagStatusFilter  `json:"filter"`
}

type batchGetImageRequest struct {
	RepositoryName     string            `json:"repositoryName"`
	RegistryID         string            `json:"registryId"`
	ImageIds           []imageIdentifier `json:"imageIds"`
	AcceptedMediaTypes []string          `json:"acceptedMediaTypes"`
}

type putImageRequest struct {
	RepositoryName         string `json:"repositoryName"`
	RegistryID             string `json:"registryId"`
	ImageManifest          string `json:"imageManifest"`
	ImageManifestMediaType string `json:"imageManifestMediaType"`
	ImageTag               string `json:"imageTag"`
	ImageDigest            string `json:"imageDigest"`
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
	if err := handlers_ecr.ValidateRepoName(name); err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if registryID != "" && registryID != accountID {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

func decodeJSONBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// handleListImages returns every imageId in the repo, honoring an optional
// TAGGED/UNTAGGED filter. Tagged manifests yield one entry per tag; untagged
// manifests yield a digest-only entry.
func (gw *GatewayConfig) handleListImages(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	var req listImagesRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return err
	}
	if err := validateRepoAndRegistry(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return err
	}

	records, err := gw.ecrListImages(ctx, accountID, req.RepositoryName)
	if err != nil {
		return err
	}

	status := ""
	if req.Filter != nil {
		status = req.Filter.TagStatus
	}
	ids := make([]*ecr.ImageIdentifier, 0, len(records))
	for _, rec := range records {
		if len(rec.Tags) == 0 {
			if status == ecr.TagStatusTagged {
				continue
			}
			ids = append(ids, &ecr.ImageIdentifier{ImageDigest: aws.String(rec.Digest)})
			continue
		}
		if status == ecr.TagStatusUntagged {
			continue
		}
		for _, tag := range rec.Tags {
			ids = append(ids, &ecr.ImageIdentifier{ImageDigest: aws.String(rec.Digest), ImageTag: aws.String(tag)})
		}
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.ListImagesOutput{ImageIds: ids})
	return nil
}

// handleDescribeImages returns detailed metadata for the repo's images,
// optionally narrowed to the requested imageIds.
func (gw *GatewayConfig) handleDescribeImages(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	var req describeImagesRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return err
	}
	if err := validateRepoAndRegistry(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return err
	}

	records, err := gw.ecrListImages(ctx, accountID, req.RepositoryName)
	if err != nil {
		return err
	}

	wanted := func(rec gateway_ecr.ImageRecord) bool {
		if len(req.ImageIds) == 0 {
			return true
		}
		for _, id := range req.ImageIds {
			if id.ImageDigest != "" && id.ImageDigest == rec.Digest {
				return true
			}
			if id.ImageTag != "" && slices.Contains(rec.Tags, id.ImageTag) {
				return true
			}
		}
		return false
	}

	details := make([]*ecr.ImageDetail, 0, len(records))
	for _, rec := range records {
		if !wanted(rec) {
			continue
		}
		detail := &ecr.ImageDetail{
			RegistryId:             aws.String(accountID),
			RepositoryName:         aws.String(req.RepositoryName),
			ImageDigest:            aws.String(rec.Digest),
			ImageSizeInBytes:       aws.Int64(rec.Size),
			ImageManifestMediaType: aws.String(rec.MediaType),
		}
		if !rec.PushedAt.IsZero() {
			detail.ImagePushedAt = aws.Time(rec.PushedAt)
		}
		for _, tag := range rec.Tags {
			detail.ImageTags = append(detail.ImageTags, aws.String(tag))
		}
		details = append(details, detail)
	}

	if len(req.ImageIds) > 0 && len(details) == 0 {
		return errors.New(awserrors.ErrorImageNotFound)
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.DescribeImagesOutput{ImageDetails: details})
	return nil
}

// handleBatchGetImage returns verbatim manifest bytes for up to 100 imageIds.
// Per Q14 it answers HTTP 200 with a structured failures array on partial
// misses; the digest wins over the tag when both are supplied.
func (gw *GatewayConfig) handleBatchGetImage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	var req batchGetImageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return err
	}
	if err := validateRepoAndRegistry(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return err
	}
	if len(req.ImageIds) > maxImageBatch {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var images []*ecr.Image
	var failures []*ecr.ImageFailure
	for _, id := range req.ImageIds {
		ref := id.ImageDigest
		if ref == "" {
			ref = id.ImageTag
		}
		if ref == "" {
			failures = append(failures, imageFailure(id, ecr.ImageFailureCodeMissingDigestAndTag, "no digest or tag specified"))
			continue
		}

		body, mediaType, digest, gErr := gw.ECRRegistry.GetManifest(ctx, accountID, req.RepositoryName, ref, req.AcceptedMediaTypes)
		if errors.Is(gErr, gateway_ecr.ErrImageNotFound) {
			failures = append(failures, imageFailure(id, ecr.ImageFailureCodeImageNotFound, "image not found"))
			continue
		}
		if gErr != nil {
			slog.ErrorContext(ctx, "BatchGetImage: get manifest failed", "repo", req.RepositoryName, "err", gErr)
			return errors.New(awserrors.ErrorServerInternal)
		}

		out := &ecr.Image{
			RegistryId:             aws.String(accountID),
			RepositoryName:         aws.String(req.RepositoryName),
			ImageId:                &ecr.ImageIdentifier{ImageDigest: aws.String(digest)},
			ImageManifest:          aws.String(string(body)),
			ImageManifestMediaType: aws.String(mediaType),
		}
		if id.ImageTag != "" {
			out.ImageId.ImageTag = aws.String(id.ImageTag)
		}
		images = append(images, out)
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.BatchGetImageOutput{Images: images, Failures: failures})
	return nil
}

// handlePutImage stores a manifest (and tag) supplied as JSON, the control-plane
// twin of an OCI manifest PUT. Used by `aws ecr put-image` to re-tag or copy an
// existing manifest.
func (gw *GatewayConfig) handlePutImage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, err := gw.ecrImageAccount(r)
	if err != nil {
		return err
	}
	var req putImageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		return err
	}
	if err := validateRepoAndRegistry(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return err
	}
	if req.ImageManifest == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	ref := req.ImageTag
	if ref == "" {
		ref = req.ImageDigest
	}

	digest, sErr := gw.ECRRegistry.StoreManifest(ctx, accountID, req.RepositoryName, ref, req.ImageManifestMediaType, []byte(req.ImageManifest))
	if sErr != nil {
		return mapStoreManifestError(ctx, sErr, req.RepositoryName)
	}

	image := &ecr.Image{
		RegistryId:             aws.String(accountID),
		RepositoryName:         aws.String(req.RepositoryName),
		ImageId:                &ecr.ImageIdentifier{ImageDigest: aws.String(digest)},
		ImageManifest:          aws.String(req.ImageManifest),
		ImageManifestMediaType: aws.String(req.ImageManifestMediaType),
	}
	if req.ImageTag != "" {
		image.ImageId.ImageTag = aws.String(req.ImageTag)
	}
	gateway_ecrapi.WriteJSONResponse(w, &ecr.PutImageOutput{Image: image})
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
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var deleted []*ecr.ImageIdentifier
	var failures []*ecr.ImageFailure
	for _, id := range req.ImageIds {
		if id.ImageDigest == "" && id.ImageTag == "" {
			failures = append(failures, imageFailure(id, ecr.ImageFailureCodeMissingDigestAndTag, "no digest or tag specified"))
			continue
		}

		digest, dErr := gw.ECRRegistry.DeleteImage(ctx, accountID, req.RepositoryName, id.ImageTag, id.ImageDigest)
		if errors.Is(dErr, gateway_ecr.ErrImageNotFound) {
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

	gateway_ecrapi.WriteJSONResponse(w, &ecr.BatchDeleteImageOutput{ImageIds: deleted, Failures: failures})
	return nil
}

// ecrListImages resolves the repo's image records, mapping a missing repo to
// RepositoryNotFound and any other backend fault to ServerInternal.
func (gw *GatewayConfig) ecrListImages(ctx context.Context, accountID, repo string) ([]gateway_ecr.ImageRecord, error) {
	records, err := gw.ECRRegistry.ListImages(ctx, accountID, repo)
	if errors.Is(err, handlers_ecr.ErrNotFound) {
		return nil, errors.New(awserrors.ErrorRepositoryNotFound)
	}
	if err != nil {
		slog.ErrorContext(ctx, "ECR image action: list images failed", "repo", repo, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return records, nil
}

// mapStoreManifestError translates the OCI manifest-store error codes into AWS
// PutImage error codes.
func mapStoreManifestError(ctx context.Context, err error, repo string) error {
	var mErr *gateway_ecr.ManifestStoreError
	if errors.As(err, &mErr) {
		switch mErr.Code {
		case "DIGEST_INVALID":
			return errors.New(awserrors.ErrorImageDigestDoesNotMatch)
		case "MANIFEST_BLOB_UNKNOWN":
			return errors.New(awserrors.ErrorLayersNotFound)
		case "TAG_IMMUTABLE":
			return errors.New(awserrors.ErrorImageTagAlreadyExists)
		case "NAME_UNKNOWN":
			return errors.New(awserrors.ErrorRepositoryNotFound)
		default:
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	slog.ErrorContext(ctx, "PutImage: store manifest failed", "repo", repo, "err", err)
	return errors.New(awserrors.ErrorServerInternal)
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
