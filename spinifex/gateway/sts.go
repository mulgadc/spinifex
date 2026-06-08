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

// stsHandler creates a type-safe STSHandler that allocates the typed input
// struct, parses query params into it, dispatches to the inner handler, and
// marshals the output to XML. Mirrors iamHandler in iam.go.
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
	// AssumeRoleWithWebIdentity is anonymous on AWS — the caller is authenticated
	// by the JWT body, not by a SigV4 envelope. anonymousSTSInterceptor routes it
	// to this dispatcher ahead of the SigV4 middleware, so STS_Request enters with
	// a zero stsCaller (see anonymousSTSActions); the handler ignores it and
	// gates the request on the token + the role's web-identity trust policy.
	"AssumeRoleWithWebIdentity": stsHandler(func(_ stsCaller, input *sts.AssumeRoleWithWebIdentityInput, gw *GatewayConfig) (any, error) {
		return gateway_sts.AssumeRoleWithWebIdentity(input, gw.STSService)
	}),
	"GetCallerIdentity": stsHandler(func(c stsCaller, input *sts.GetCallerIdentityInput, gw *GatewayConfig) (any, error) {
		return gateway_sts.GetCallerIdentity(c.accountID, c.arn, c.principalType, c.identity, c.assumedRoleID, input, gw.IAMService, gw.STSService)
	}),
}

// stsSkipPolicyCheck lists the actions whose authorization is NOT gated by an
// IAM policy check on the caller. AssumeRole is gated by the target role's
// trust policy (evaluated inside the handler); GetCallerIdentity is always
// allowed per AWS so SDK init flows do not break;
// AssumeRoleWithWebIdentity is anonymous — caller is identified by the JWT,
// not by SigV4 — and trust-policy-gated inside the handler.
var stsSkipPolicyCheck = map[string]bool{
	"AssumeRole":                true,
	"AssumeRoleWithWebIdentity": true,
	"GetCallerIdentity":         true,
}

// anonymousSTSActions lists STS actions AWS accepts without SigV4: the caller
// holds no AWS credentials and is authenticated solely by a web-identity JWT in
// the request body. anonymousSTSInterceptor routes these ahead of the SigV4
// middleware, and STS_Request skips SigV4-caller resolution for them.
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

	// Anonymous actions (AssumeRoleWithWebIdentity) carry no SigV4 envelope, so
	// there is no caller to resolve — the handler ignores it.
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
// Pulls the principal-type set by auth.go's prefix-first dispatch (user vs.
// assumed-role) and constructs the caller ARN — including the chained-assume
// case where the ARN is already the AssumedRoleARN.
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

// buildCallerARN composes the caller ARN per AWS conventions:
//
//   - assumed-role → ctxAssumedRoleARN (the arn:aws:sts::A:assumed-role/R/S
//     form set by the SigV4 verifier).
//   - root         → arn:aws:iam::{aid}:root (matches AWS where the root
//     principal does not use the user/ subpath).
//   - user         → arn:aws:iam::{aid}:user/{identity}.
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
