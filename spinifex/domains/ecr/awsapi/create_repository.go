package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// createRepositoryRequest is the camelCase AWS JSON 1.1 input shape. ecr.Tag
// carries no locationName, so its wire keys are Key/Value (capitalised); using
// the SDK type preserves that spelling rather than reimplementing it.
type createRepositoryRequest struct {
	RepositoryName             string                           `json:"repositoryName"`
	RegistryID                 string                           `json:"registryId"`
	ImageTagMutability         string                           `json:"imageTagMutability"`
	Tags                       []*ecr.Tag                       `json:"tags"`
	EncryptionConfiguration    *encryptionConfigurationInput    `json:"encryptionConfiguration"`
	ImageScanningConfiguration *imageScanningConfigurationInput `json:"imageScanningConfiguration"`
}

// encryptionConfigurationInput is the camelCase input shape for
// encryptionConfiguration. kmsKey is not decoded: KMS is rejected outright,
// so any key supplied alongside it is never used.
type encryptionConfigurationInput struct {
	EncryptionType string `json:"encryptionType"`
}

// CreateRepository creates only the account-scoped ECR metadata record. The
// Predastore bucket remains lazy until the first image push. An omitted
// mutability value defaults to MUTABLE and an omitted encryption type defaults
// to AES256.
//
// The advertised endpoint is deployment composition rather than ECR resource
// state, so the caller supplies it to project the AWS response.
func CreateRepository(ctx context.Context, store RepositoryStore, endpoint RepositoryEndpoint, accountID string, body []byte) (*ecr.CreateRepositoryOutput, error) {
	if store == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var req createRepositoryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryName(req.RepositoryName); err != nil {
		return nil, err
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	mutability, err := normalizeTagMutability(req.ImageTagMutability)
	if err != nil {
		return nil, err
	}
	encryptionType, err := normalizeEncryptionType(req.EncryptionConfiguration)
	if err != nil {
		return nil, err
	}
	tags, err := tagMapFromInput(req.Tags)
	if err != nil {
		return nil, err
	}

	if _, err := store.GetRepo(ctx, accountID, req.RepositoryName); err == nil {
		return nil, RepositoryAlreadyExistsError(accountID, req.RepositoryName)
	} else if !errors.Is(err, handlers_ecr.ErrNotFound) {
		slog.ErrorContext(ctx, "ECR CreateRepository: get repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	meta := handlers_ecr.RepoMeta{
		Name:               req.RepositoryName,
		CreatedAt:          time.Now().UTC(),
		ImageTagMutability: mutability,
		EncryptionType:     encryptionType,
		ScanOnPush:         req.ImageScanningConfiguration != nil && req.ImageScanningConfiguration.ScanOnPush,
		Tags:               tags,
	}
	if err := store.PutRepo(ctx, accountID, meta); err != nil {
		slog.ErrorContext(ctx, "ECR CreateRepository: put repository failed", "repository", req.RepositoryName, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &ecr.CreateRepositoryOutput{
		Repository: endpoint.RepositoryFromMeta(accountID, req.RepositoryName, meta),
	}, nil
}

// tagMapFromInput folds the create-time tag list into RepoMeta's map, rejecting
// an empty key exactly as TagResource does. An absent list is nil so the record
// round-trips identically to one created before tags were supported.
func tagMapFromInput(in []*ecr.Tag) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if err := ValidateTags(in); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(in))
	for _, tag := range in {
		out[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return out, nil
}

// normalizeEncryptionType validates the requested encryption configuration,
// defaulting an absent one to AES256. KMS is rejected because no
// customer-managed key material is implemented.
func normalizeEncryptionType(cfg *encryptionConfigurationInput) (string, error) {
	if cfg == nil || cfg.EncryptionType == "" {
		return handlers_ecr.EncryptionTypeAES256, nil
	}
	switch cfg.EncryptionType {
	case handlers_ecr.EncryptionTypeAES256:
		return cfg.EncryptionType, nil
	case handlers_ecr.EncryptionTypeKMS:
		return "", awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
			"encryptionType KMS is not supported: no customer-managed key is used, and repositories are already encrypted at rest under a server-managed AES-256 key")
	default:
		return "", EnumValueError("encryptionConfiguration.encryptionType",
			handlers_ecr.EncryptionTypeAES256, "KMS_DSSE", handlers_ecr.EncryptionTypeKMS)
	}
}

// normalizeTagMutability validates the create-time mutability, defaulting an
// empty value to MUTABLE. An unknown value is rejected with
// InvalidParameterException.
func normalizeTagMutability(value string) (string, error) {
	switch value {
	case "":
		return handlers_ecr.TagMutabilityMutable, nil
	case handlers_ecr.TagMutabilityMutable, handlers_ecr.TagMutabilityImmutable:
		return value, nil
	default:
		return "", EnumValueError("imageTagMutability", ImageTagMutabilityValues...)
	}
}
