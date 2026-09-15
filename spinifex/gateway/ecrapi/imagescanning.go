package gateway_ecrapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/nats-io/nats.go"
)

// putImageScanningConfigurationRequest is the camelCase AWS JSON 1.1 input
// shape. The SDK input struct carries locationName tags rather than json
// tags, so the subset honored here is decoded explicitly.
type putImageScanningConfigurationRequest struct {
	RepositoryName             string                           `json:"repositoryName"`
	RegistryID                 string                           `json:"registryId"`
	ImageScanningConfiguration *imageScanningConfigurationInput `json:"imageScanningConfiguration"`
}

// imageScanningConfigurationInput is the camelCase input shape for
// imageScanningConfiguration.
type imageScanningConfigurationInput struct {
	ScanOnPush bool `json:"scanOnPush"`
}

// PutImageScanningConfiguration accepts the honest, already-true state —
// scanOnPush unset or false — as a no-op. scanOnPush=true asks for a scanner
// mulga does not run and is rejected with OperationNotSupportedException.
func PutImageScanningConfiguration(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error) {
	var req putImageScanningConfigurationRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	if req.RepositoryName == "" || handlers_ecr.ValidateRepoName(req.RepositoryName) != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	if req.ImageScanningConfiguration != nil && req.ImageScanningConfiguration.ScanOnPush {
		return nil, errors.New(awserrors.ErrorOperationNotSupported)
	}

	store := handlers_ecr.NewNATSMetaStore(nc)
	meta, err := store.GetRepo(ctx, accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return nil, errors.New(awserrors.ErrorRepositoryNotFound)
		}
		return nil, err
	}
	meta.ScanOnPush = false
	if err := store.PutRepo(ctx, accountID, meta); err != nil {
		return nil, err
	}
	return &ecr.PutImageScanningConfigurationOutput{
		RegistryId:     aws.String(accountID),
		RepositoryName: aws.String(req.RepositoryName),
		ImageScanningConfiguration: &ecr.ImageScanningConfiguration{
			ScanOnPush: aws.Bool(false),
		},
	}, nil
}
