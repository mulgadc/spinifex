package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
)

// ecrActionFromTarget extracts the action suffix from an X-Amz-Target header.
// Any "<Prefix>.<Action>" or bare "<Action>" form is accepted.
func ecrActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// ECR_Request dispatches AWS JSON 1.1 ECR control-plane requests. The action
// comes from X-Amz-Target; errors are returned as awserrors codes and rendered
// by the shared ErrorHandler.
func (gw *GatewayConfig) ECR_Request(w http.ResponseWriter, r *http.Request) error {
	action := ecrActionFromTarget(r.Header.Get("X-Amz-Target"))
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := gateway_ecrapi.Actions[action]
	if !ok {
		slog.Debug("ECR: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "ecr", action); err != nil {
		return err
	}

	// GetAuthorizationToken mints a registry token from the SigV4 auth context;
	// it is served inline rather than relayed onto a NATS subject.
	if action == "GetAuthorizationToken" {
		return gw.handleGetAuthorizationToken(w, r)
	}

	// DescribeRepositories builds repository ARNs/URIs from the gateway region
	// and internal suffix, which the relayed Handler signature does not carry,
	// so it is served inline like GetAuthorizationToken.
	if action == "DescribeRepositories" {
		return gw.handleDescribeRepositories(w, r)
	}

	// CreateRepository and DeleteRepository return the repository ARN/URI, built
	// from the gateway region and internal suffix, so they are served inline.
	if action == "CreateRepository" {
		return gw.handleCreateRepository(w, r)
	}
	if action == "DeleteRepository" {
		return gw.handleDeleteRepository(w, r)
	}

	// PutImageTagMutability read-modify-writes the per-account RepoMeta record
	// over NATS, like CreateRepository, so it is served inline.
	if action == "PutImageTagMutability" {
		return gw.handlePutImageTagMutability(w, r)
	}

	// Image-management actions need the gateway-side predastore Store (manifest
	// bytes) carried by the OCI Registry, which the relayed Handler signature
	// does not reach, so they are served inline.
	switch action {
	case "ListImages":
		return gw.handleListImages(w, r)
	case "DescribeImages":
		return gw.handleDescribeImages(w, r)
	case "BatchGetImage":
		return gw.handleBatchGetImage(w, r)
	case "PutImage":
		return gw.handlePutImage(w, r)
	case "BatchDeleteImage":
		return gw.handleBatchDeleteImage(w, r)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("ECR_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("ECR_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	output, err := handler(gw.NATSConn, accountID, body)
	if err != nil {
		return err
	}

	gateway_ecrapi.WriteJSONResponse(w, output)
	return nil
}
