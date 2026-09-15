package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// createRepositoryRequest is the camelCase AWS JSON 1.1 input shape. Tags are
// set via a follow-on TagResource call by real clients, matching AWS, so they
// are not accepted here.
type createRepositoryRequest struct {
	RepositoryName             string                           `json:"repositoryName"`
	RegistryID                 string                           `json:"registryId"`
	ImageTagMutability         string                           `json:"imageTagMutability"`
	EncryptionConfiguration    *encryptionConfigurationInput    `json:"encryptionConfiguration"`
	ImageScanningConfiguration *imageScanningConfigurationInput `json:"imageScanningConfiguration"`
}

// encryptionConfigurationInput is the camelCase input shape for
// encryptionConfiguration. kmsKey is not decoded: KMS is rejected outright, so
// any key material supplied alongside it is never read.
type encryptionConfigurationInput struct {
	EncryptionType string `json:"encryptionType"`
}

// imageScanningConfigurationInput is the camelCase input shape for
// imageScanningConfiguration.
type imageScanningConfigurationInput struct {
	ScanOnPush bool `json:"scanOnPush"`
}

// handleCreateRepository provisions an empty repository in the caller account.
// The predastore object bucket stays lazily created on first push; this writes
// only the per-account KV meta record. imageTagMutability (MUTABLE/IMMUTABLE)
// is validated, persisted, and echoed; an unset value defaults to MUTABLE.
func (gw *GatewayConfig) handleCreateRepository(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, _ := ctx.Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(ctx, "CreateRepository: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(ctx, "CreateRepository: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	var req createRepositoryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := handlers_ecr.ValidateRepoName(req.RepositoryName); err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	mutability, err := normalizeTagMutability(req.ImageTagMutability)
	if err != nil {
		return err
	}
	encryptionType, err := normalizeEncryptionType(req.EncryptionConfiguration)
	if err != nil {
		return err
	}
	scanOnPush := req.ImageScanningConfiguration != nil && req.ImageScanningConfiguration.ScanOnPush

	store := handlers_ecr.NewNATSMetaStore(gw.NATSConn)
	if _, err := store.GetRepo(ctx, accountID, req.RepositoryName); err == nil {
		return errors.New(awserrors.ErrorRepositoryAlreadyExists)
	} else if !errors.Is(err, handlers_ecr.ErrNotFound) {
		slog.ErrorContext(ctx, "CreateRepository: get repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	meta := handlers_ecr.RepoMeta{
		Name:               req.RepositoryName,
		CreatedAt:          time.Now().UTC(),
		ImageTagMutability: mutability,
		EncryptionType:     encryptionType,
		ScanOnPush:         scanOnPush,
	}
	if err := store.PutRepo(ctx, accountID, meta); err != nil {
		slog.ErrorContext(ctx, "CreateRepository: put repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.CreateRepositoryOutput{
		Repository: gw.buildRepository(accountID, req.RepositoryName, meta),
	})
	return nil
}

// normalizeEncryptionType validates the requested encryption configuration,
// defaulting an absent one to AES256. KMS is rejected: no customer key
// material is plumbed anywhere for it to honor.
func normalizeEncryptionType(cfg *encryptionConfigurationInput) (string, error) {
	if cfg == nil || cfg.EncryptionType == "" {
		return handlers_ecr.EncryptionTypeAES256, nil
	}
	switch cfg.EncryptionType {
	case handlers_ecr.EncryptionTypeAES256:
		return cfg.EncryptionType, nil
	case handlers_ecr.EncryptionTypeKMS:
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"encryptionType KMS is not supported: no customer-managed key is used, and repositories are already encrypted at rest under a server-managed AES-256 key")
	default:
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
}

// normalizeTagMutability validates the requested mutability, defaulting an empty
// value to MUTABLE. An unknown value is rejected with InvalidParameterValue.
func normalizeTagMutability(v string) (string, error) {
	switch v {
	case "":
		return handlers_ecr.TagMutabilityMutable, nil
	case handlers_ecr.TagMutabilityMutable, handlers_ecr.TagMutabilityImmutable:
		return v, nil
	default:
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
}
