package gateway

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
)

// handleGetAuthorizationToken mints a self-contained ES256 ECR token for the
// SigV4-authenticated caller and returns it in the AWS GetAuthorizationToken
// shape. The token is base64("AWS:<jwt>"); docker stores it and replays it as
// Basic auth against the /v2 registry, where the auth bridge verifies it.
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
	principalType, _ := ctx.Value(ctxPrincipalType).(string)

	token, expiresAt, err := gw.ECRTokenIssuer.Mint(gateway_ecrauth.Principal{
		AccountID:   accountID,
		ARN:         eksCallerPrincipalARN(r),
		Type:        principalType,
		AccessKeyID: accessKey,
	})
	if err != nil {
		slog.Error("GetAuthorizationToken: mint failed", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	authToken := base64.StdEncoding.EncodeToString([]byte("AWS:" + token))
	proxyEndpoint := "https://" + accountID + ".dkr.ecr." + gw.Region + "." + gw.InternalSuffix

	gateway_ecrapi.WriteJSONResponse(w, &ecr.GetAuthorizationTokenOutput{
		AuthorizationData: []*ecr.AuthorizationData{{
			AuthorizationToken: aws.String(authToken),
			ProxyEndpoint:      aws.String(proxyEndpoint),
			ExpiresAt:          aws.Time(expiresAt),
		}},
	})
	return nil
}
