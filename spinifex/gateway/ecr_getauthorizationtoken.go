package gateway

import (
	"errors"
	"log/slog"
	"net/http"

	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// handleGetAuthorizationToken adapts an authenticated gateway identity into
// the canonical ECR credential principal. The AWS token response is owned by
// the ECR action adapter, while canonical IAM/STS ARN construction remains a
// generic gateway identity concern.
func (gw *GatewayConfig) handleGetAuthorizationToken(w http.ResponseWriter, r *http.Request) error {
	if gw.ECRTokenIssuer == nil {
		return errors.New(awserrors.ErrorNotImplemented)
	}
	ctx := r.Context()
	accountID, _ := ctx.Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("GetAuthorizationToken: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}
	accessKey, _ := ctx.Value(ctxAccessKey).(string)
	identity, _ := ctx.Value(ctxIdentity).(string)
	principalType, _ := ctx.Value(ctxPrincipalType).(string)
	assumedRoleARN, _ := ctx.Value(ctxAssumedRoleARN).(string)

	// The minted token names the exact IAM/STS record every /v2/* request will
	// later rehydrate against, so its subject must be the same canonical ARN
	// buildCallerARN produces everywhere else in the gateway (STS included) —
	// not a best-effort approximation that could omit an IAM path.
	callerARN, err := buildCallerARN(accountID, identity, principalType, assumedRoleARN)
	if err != nil {
		slog.Error("GetAuthorizationToken: cannot build canonical caller ARN", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	output, err := awsapi.GetAuthorizationToken(gw.ECRTokenIssuer, gw.ecrRepositoryEndpoint(), ecrauth.Principal{
		AccountID:   accountID,
		ARN:         callerARN,
		Type:        principalType,
		AccessKeyID: accessKey,
	})
	if err != nil {
		slog.Error("GetAuthorizationToken: action failed", "err", err)
		return err
	}

	awsapi.WriteJSONResponse(w, output)
	return nil
}
