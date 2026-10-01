package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// describeRepositoriesRequest is the camelCase AWS JSON 1.1 input shape. The SDK
// input struct carries locationName tags rather than json tags, so the subset
// 2e honors is decoded explicitly.
type describeRepositoriesRequest struct {
	RegistryID      string   `json:"registryId"`
	RepositoryNames []string `json:"repositoryNames"`
}

// handleDescribeRepositories lists the caller account's repositories. Scope is
// the caller account; a registryId naming a different account is the Q8 parity
// gap pending registry-policy v2 and is denied. Pagination (maxResults/
// nextToken, Q9) is not yet implemented — the full list is returned in one page.
func (gw *GatewayConfig) handleDescribeRepositories(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, _ := ctx.Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(ctx, "DescribeRepositories: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeRepositories: failed to read body", "err", err)
		return awsapi.MalformedBodyError()
	}
	var req describeRepositoriesRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return awsapi.MalformedBodyError()
		}
	}
	if req.RegistryID != "" && req.RegistryID != accountID {
		return errors.New(awserrors.ErrorAccessDenied)
	}

	store := handlers_ecr.NewNATSMetaStore(gw.NATSConn)
	names := req.RepositoryNames
	if len(names) == 0 {
		names, err = store.ListRepos(ctx, accountID)
		if err != nil {
			slog.ErrorContext(ctx, "DescribeRepositories: list repos failed", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	repos := make([]*ecr.Repository, 0, len(names))
	for _, name := range names {
		meta, err := store.GetRepo(ctx, accountID, name)
		if err != nil {
			if errors.Is(err, handlers_ecr.ErrNotFound) {
				return errors.New(awserrors.ErrorRepositoryNotFound)
			}
			slog.ErrorContext(ctx, "DescribeRepositories: get repo failed", "repo", name, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		repos = append(repos, gw.ecrRepositoryEndpoint().RepositoryFromMeta(accountID, name, meta))
	}

	awsapi.WriteJSONResponse(w, &ecr.DescribeRepositoriesOutput{Repositories: repos})
	return nil
}
