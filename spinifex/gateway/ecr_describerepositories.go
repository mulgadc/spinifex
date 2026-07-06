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
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	var req describeRepositoriesRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return errors.New(awserrors.ErrorInvalidParameterValue)
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
		repos = append(repos, &ecr.Repository{
			RegistryId:         aws.String(accountID),
			RepositoryName:     aws.String(name),
			RepositoryArn:      aws.String(gw.ecrRepositoryArn(accountID, name)),
			RepositoryUri:      aws.String(gw.ecrRepositoryUri(accountID, name)),
			CreatedAt:          aws.Time(meta.CreatedAt),
			ImageTagMutability: aws.String(meta.TagMutability()),
		})
	}

	gateway_ecrapi.WriteJSONResponse(w, &ecr.DescribeRepositoriesOutput{Repositories: repos})
	return nil
}

// ecrRepositoryArn builds the ECR repository ARN for an account-scoped repo.
func (gw *GatewayConfig) ecrRepositoryArn(accountID, name string) string {
	return "arn:aws:ecr:" + gw.Region + ":" + accountID + ":repository/" + name
}

// ecrRegistryHost builds the registry host, appending the gateway's advertised
// port so docker dials the right port (omitted for 443). When the gateway
// advertises a reachable host it is used directly — the account comes from the
// auth token, so docker needs no DNS; otherwise the per-account
// <acct>.dkr.ecr.<region>.<suffix> parity name is used.
func (gw *GatewayConfig) ecrRegistryHost(accountID string) string {
	host := gw.RegistryHost
	if host == "" {
		host = accountID + ".dkr.ecr." + gw.Region + "." + gw.InternalSuffix
	}
	if gw.RegistryPort != "" && gw.RegistryPort != "443" {
		return host + ":" + gw.RegistryPort
	}
	return host
}

// ecrRepositoryUri builds the registry pull/push URI for an account-scoped repo.
func (gw *GatewayConfig) ecrRepositoryUri(accountID, name string) string {
	return gw.ecrRegistryHost(accountID) + "/" + name
}
