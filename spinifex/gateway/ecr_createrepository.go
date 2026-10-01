package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// handleCreateRepository adapts the authenticated HTTP request to the ECR AWS
// action. ECR request semantics and resource state are domain-adapter
// responsibilities; gateway retains HTTP and auth-context handling.
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
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.CreateRepository(ctx, gw.NATSConn, gw.ecrRepositoryEndpoint(), accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}
