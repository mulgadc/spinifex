package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_sts "github.com/mulgadc/spinifex/spinifex/gateway/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// stsCaller bundles the SigV4-derived caller fields that any STS action may
// consume. The dispatcher builds this once per request so handlers stay free
// of context-key plumbing.
type stsCaller struct {
	accountID      string
	arn            string
	identity       string
	principalType  string
	assumedRoleARN string
	assumedRoleID  string
	accessKey      string
}

// STSHandler processes parsed query args and returns XML response bytes.
type STSHandler func(action string, q map[string]string, gw *GatewayConfig, c stsCaller) ([]byte, error)

// stsHandler creates a type-safe STSHandler: allocates the input struct, parses
// query params, dispatches to the inner handler, and marshals output to XML.
func stsHandler[In any](handler func(c stsCaller, input *In, gw *GatewayConfig) (any, error)) STSHandler {
	return func(action string, q map[string]string, gw *GatewayConfig, c stsCaller) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, errors.New(awserrors.ErrorValidationError)
		}
		output, err := handler(c, input, gw)
		if err != nil {
			return nil, err
		}
		payload := utils.GenerateIAMXMLPayload(action, output)
		xmlOutput, err := utils.MarshalToXML(payload)
		if err != nil {
			return nil, errors.New(awserrors.ErrorInternalError)
		}
		return xmlOutput, nil
	}
}

var stsActions = map[string]STSHandler{
	"AssumeRole": stsHandler(func(c stsCaller, input *sts.AssumeRoleInput, gw *GatewayConfig) (any, error) {
		return gateway_sts.AssumeRole(c.accountID, c.arn, c.identity, input, gw.STSService)
	}),
	// AssumeRoleWithWebIdentity is anonymous — authenticated by JWT, not SigV4.
	// anonymousSTSInterceptor routes it ahead of auth with a zero stsCaller.
	"AssumeRoleWithWebIdentity": stsHandler(func(_ stsCaller, input *sts.AssumeRoleWithWebIdentityInput, gw *GatewayConfig) (any, error) {
		return gateway_sts.AssumeRoleWithWebIdentity(input, gw.STSService)
	}),
	"GetCallerIdentity": stsHandler(func(c stsCaller, input *sts.GetCallerIdentityInput, gw *GatewayConfig) (any, error) {
		return gateway_sts.GetCallerIdentity(c.accountID, c.arn, c.principalType, c.identity, c.assumedRoleID, input, gw.IAMService, gw.STSService)
	}),
	"GetSessionToken": stsHandler(func(c stsCaller, input *sts.GetSessionTokenInput, gw *GatewayConfig) (any, error) {
		return gateway_sts.GetSessionToken(c.accountID, c.identity, c.principalType, c.accessKey, input, gw.STSService)
	}),
}

// stsSkipPolicyCheck lists actions not gated by IAM policy on the caller.
// AssumeRole is gated by the role's trust policy in the handler.
// GetCallerIdentity is always allowed per AWS (SDK init flows must not break).
// AssumeRoleWithWebIdentity is anonymous; trust-policy-gated in the handler.
// GetSessionToken is allowed for any authenticated caller; assumed-role and
// session (ASIA) callers are rejected inside the handler.
var stsSkipPolicyCheck = map[string]bool{
	"AssumeRole":                true,
	"AssumeRoleWithWebIdentity": true,
	"GetCallerIdentity":         true,
	"GetSessionToken":           true,
}

// anonymousSTSActions lists STS actions that carry no SigV4; authenticated by
// a web-identity JWT instead. anonymousSTSInterceptor routes these before auth.
var anonymousSTSActions = map[string]bool{
	"AssumeRoleWithWebIdentity": true,
}

func (gw *GatewayConfig) STS_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("STS: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	handler, ok := stsActions[action]
	if !ok {
		slog.Debug("STS: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if gw.STSService == nil {
		slog.Error("STS: service not initialized")
		return errors.New(awserrors.ErrorInternalError)
	}

	if !stsSkipPolicyCheck[action] {
		if err := gw.checkPolicy(r, "sts", action); err != nil {
			return err
		}
	}

	// Anonymous actions carry no SigV4 envelope; handler ignores the zero caller.
	var caller stsCaller
	if !anonymousSTSActions[action] {
		caller, err = gw.resolveSTSCaller(r)
		if err != nil {
			return err
		}
	}

	xmlOutput, err := handler(action, queryArgs, gw, caller)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.Error("Failed to write STS response", "err", err)
	}
	return nil
}

// resolveSTSCaller assembles the caller fields from the SigV4 request context.
func (gw *GatewayConfig) resolveSTSCaller(r *http.Request) (stsCaller, error) {
	ctx := r.Context()
	accountID, _ := ctx.Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("STS_Request: no account ID in auth context")
		return stsCaller{}, errors.New(awserrors.ErrorInternalError)
	}
	identity, _ := ctx.Value(ctxIdentity).(string)
	principalType, _ := ctx.Value(ctxPrincipalType).(string)
	assumedRoleARN, _ := ctx.Value(ctxAssumedRoleARN).(string)
	assumedRoleID, _ := ctx.Value(ctxAssumedRoleID).(string)
	accessKey, _ := ctx.Value(ctxAccessKey).(string)

	arn, err := buildCallerARN(accountID, identity, principalType, assumedRoleARN)
	if err != nil {
		return stsCaller{}, err
	}
	return stsCaller{
		accountID:      accountID,
		arn:            arn,
		identity:       identity,
		principalType:  principalType,
		assumedRoleARN: assumedRoleARN,
		assumedRoleID:  assumedRoleID,
		accessKey:      accessKey,
	}, nil
}

// buildCallerARN composes the caller ARN: assumed-role uses ctxAssumedRoleARN,
// root uses arn:aws:iam::{aid}:root, user uses arn:aws:iam::{aid}:user/{identity}.
func buildCallerARN(accountID, identity, principalType, assumedRoleARN string) (string, error) {
	switch principalType {
	case principalTypeAssumedRole:
		if assumedRoleARN == "" {
			slog.Error("STS_Request: assumed-role principal without ARN")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return assumedRoleARN, nil
	case principalTypeRoot:
		return fmt.Sprintf("arn:aws:iam::%s:root", accountID), nil
	case principalTypeUser:
		if identity == "root" && accountID == utils.GlobalAccountID {
			return fmt.Sprintf("arn:aws:iam::%s:root", accountID), nil
		}
		if identity == "" {
			slog.Error("STS_Request: user principal without identity")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return fmt.Sprintf("arn:aws:iam::%s:user/%s", accountID, identity), nil
	default:
		slog.Error("STS_Request: unknown principal type", "principalType", principalType)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}
