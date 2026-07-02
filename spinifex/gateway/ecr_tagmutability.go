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

// putImageTagMutabilityRequest is the camelCase AWS JSON 1.1 input shape.
type putImageTagMutabilityRequest struct {
	RepositoryName     string `json:"repositoryName"`
	RegistryID         string `json:"registryId"`
	ImageTagMutability string `json:"imageTagMutability"`
}

// handlePutImageTagMutability flips a repository between MUTABLE and IMMUTABLE.
// It read-modify-writes the per-account RepoMeta record; the value is enforced
// on push by Registry.StoreManifest.
func (gw *GatewayConfig) handlePutImageTagMutability(w http.ResponseWriter, r *http.Request) error {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("PutImageTagMutability: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("PutImageTagMutability: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	var req putImageTagMutabilityRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := handlers_ecr.ValidateRepoName(req.RepositoryName); err != nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	// imageTagMutability is required here (unlike CreateRepository's default).
	if req.ImageTagMutability != handlers_ecr.TagMutabilityMutable &&
		req.ImageTagMutability != handlers_ecr.TagMutabilityImmutable {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	store := handlers_ecr.NewNATSMetaStore(gw.NATSConn)
	meta, err := store.GetRepo(accountID, req.RepositoryName)
	if err != nil {
		if errors.Is(err, handlers_ecr.ErrNotFound) {
			return errors.New(awserrors.ErrorRepositoryNotFound)
		}
		slog.Error("PutImageTagMutability: get repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	meta.ImageTagMutability = req.ImageTagMutability
	if err := store.PutRepo(accountID, meta); err != nil {
		slog.Error("PutImageTagMutability: put repo failed", "repo", req.RepositoryName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.PutImageTagMutabilityOutput{
		RegistryId:         aws.String(accountID),
		RepositoryName:     aws.String(req.RepositoryName),
		ImageTagMutability: aws.String(meta.TagMutability()),
	})
	return nil
}
