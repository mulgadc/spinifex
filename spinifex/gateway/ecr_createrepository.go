package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// createRepositoryRequest is the camelCase AWS JSON 1.1 input shape. The SDK
// input struct carries locationName tags rather than json tags, so the subset
// honored here is decoded explicitly. Tags, encryption, and scanning config are
// accepted-and-ignored in v1.
type createRepositoryRequest struct {
	RepositoryName     string `json:"repositoryName"`
	RegistryID         string `json:"registryId"`
	ImageTagMutability string `json:"imageTagMutability"`
}

// handleCreateRepository provisions an empty repository in the caller account.
// The predastore object bucket stays lazily created on first push; this writes
// only the per-account KV meta record. imageTagMutability is reported as MUTABLE
// in v1 (immutable tags are a separate follow-on).
func (gw *GatewayConfig) handleCreateRepository(w http.ResponseWriter, r *http.Request) error {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("CreateRepository: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("CreateRepository: failed to read body", "err", err)
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

	store := handlers_ecr.NewNATSMetaStore(gw.NATSConn)
	if _, err := store.GetRepo(accountID, req.RepositoryName); err == nil {
		return errors.New(awserrors.ErrorRepositoryAlreadyExists)
	} else if !errors.Is(err, handlers_ecr.ErrNotFound) {
		slog.Error("CreateRepository: get repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	meta := handlers_ecr.RepoMeta{Name: req.RepositoryName, CreatedAt: time.Now().UTC()}
	if err := store.PutRepo(accountID, meta); err != nil {
		slog.Error("CreateRepository: put repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.CreateRepositoryOutput{
		Repository: &ecr.Repository{
			RegistryId:         aws.String(accountID),
			RepositoryName:     aws.String(req.RepositoryName),
			RepositoryArn:      aws.String(gw.ecrRepositoryArn(accountID, req.RepositoryName)),
			RepositoryUri:      aws.String(gw.ecrRepositoryUri(accountID, req.RepositoryName)),
			CreatedAt:          aws.Time(meta.CreatedAt),
			ImageTagMutability: aws.String("MUTABLE"),
		},
	})
	return nil
}
