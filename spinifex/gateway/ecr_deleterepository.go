package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// handleDeleteRepository adapts the authenticated HTTP request to the ECR AWS
// action. ECR lifecycle rules and metadata operations belong to the domain
// adapter; gateway retains HTTP and auth-context handling.
func (gw *GatewayConfig) handleDeleteRepository(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	accountID, _ := ctx.Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(ctx, "DeleteRepository: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteRepository: failed to read body", "err", err)
		return awsapi.MalformedBodyError()
	}
	output, err := awsapi.DeleteRepository(ctx, gw.NATSConn, gw.ecrRepositoryEndpoint(), accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}
