package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// deleteRepositoryRequest is the camelCase AWS JSON 1.1 input shape. force=true
// allows deleting a repository that still contains images.
type deleteRepositoryRequest struct {
	RepositoryName string `json:"repositoryName"`
	RegistryID     string `json:"registryId"`
	Force          bool   `json:"force"`
}

// handleDeleteRepository removes a repository in the caller account. Without
// force, a repository that still holds images is rejected with
// RepositoryNotEmptyException. The KV metadata (meta, policy, tags, manifests)
// is cascaded; predastore blob reclamation is deferred to a separate GC pass.
func (gw *GatewayConfig) handleDeleteRepository(w http.ResponseWriter, r *http.Request) error {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("DeleteRepository: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("DeleteRepository: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	var req deleteRepositoryRequest
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
	meta, err := store.GetRepo(accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return errors.New(awserrors.ErrorRepositoryNotFound)
		}
		slog.Error("DeleteRepository: get repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	if !req.Force {
		manifests, err := store.ListManifests(accountID, req.RepositoryName)
		if err != nil {
			slog.Error("DeleteRepository: list manifests failed", "repo", req.RepositoryName, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if len(manifests) > 0 {
			return errors.New(awserrors.ErrorRepositoryNotEmpty)
		}
	}

	if err := store.DeleteRepo(accountID, req.RepositoryName); err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return errors.New(awserrors.ErrorRepositoryNotFound)
		}
		slog.Error("DeleteRepository: delete repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.DeleteRepositoryOutput{
		Repository: &ecr.Repository{
			RegistryId:         aws.String(accountID),
			RepositoryName:     aws.String(req.RepositoryName),
			RepositoryArn:      aws.String(gw.ecrRepositoryArn(accountID, req.RepositoryName)),
			RepositoryUri:      aws.String(gw.ecrRepositoryUri(accountID, req.RepositoryName)),
			CreatedAt:          aws.Time(meta.CreatedAt),
			ImageTagMutability: aws.String(meta.TagMutability()),
		},
	})
	return nil
}
