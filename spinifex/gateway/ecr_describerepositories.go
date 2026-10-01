package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// handleDescribeRepositories adapts the authenticated HTTP request to the ECR
// AWS action. Request semantics, storage and AWS response construction live in
// the ECR domain adapter; gateway retains only HTTP and auth-context concerns.
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
	output, err := awsapi.DescribeRepositories(ctx, gw.NATSConn, gw.ecrRepositoryEndpoint(), accountID, body)
	if err != nil {
		return err
	}
	awsapi.WriteJSONResponse(w, output)
	return nil
}
