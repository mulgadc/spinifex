package gateway

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	spxarn "github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_sts "github.com/mulgadc/spinifex/spinifex/gateway/sts"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
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

// anonymousSTSActions lists STS actions that carry no SigV4; authenticated by
// a web-identity JWT instead. anonymousSTSInterceptor routes these before auth.
var anonymousSTSActions = map[string]bool{
	"AssumeRoleWithWebIdentity": true,
}

// stsPolicyGatedActions lists the actions requiring a pass on the caller's
// identity policy. GetCallerIdentity and GetSessionToken are authentication
// operations AWS requires no permission for, and AssumeRoleWithWebIdentity
// arrives with no identity to evaluate.
var stsPolicyGatedActions = map[string]bool{
	"AssumeRole": true,
}

// stsPolicyResource returns the ARN a gated action is evaluated against.
// AssumeRole resolves the target role so the resource is the ARN IAM stored,
// which a policy naming a pathed role matches and an invented path does not.
func (gw *GatewayConfig) stsPolicyResource(queryArgs map[string]string) (string, error) {
	// An absent RoleArn is a malformed request, not an unauthorized one: saying
	// so beats sending the caller to widen a policy that was never the cause.
	roleARN := queryArgs["RoleArn"]
	if roleARN == "" {
		return "", errors.New(awserrors.ErrorMissingParameter)
	}
	if gw.IAMService == nil {
		slog.Error("STS: IAM service not available", "action", "AssumeRole")
		return "", errors.New(awserrors.ErrorInternalError)
	}

	_, role, err := handlers_sts.ResolveRoleByARN(gw.IAMService, roleARN)
	switch {
	case err == nil:
		return aws.StringValue(role.Arn), nil
	case errors.Is(err, handlers_sts.ErrRoleUnresolved):
		// Echoing the supplied ARN keeps the denial identical whether or not the
		// role exists. The handler refuses an ARN that is not the stored one, so
		// a grant matched here on an invented path still ends in a denial there.
		return roleARN, nil
	}
	if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorValidationError {
		slog.Debug("STS: unparseable RoleArn", "roleArn", roleARN)
		return "", err
	}
	slog.Error("STS: role lookup failed ahead of the identity gate", "roleArn", roleARN, "err", err)
	return "", errors.New(awserrors.ErrorInternalError)
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

	// Anonymous actions carry no SigV4 envelope; handler ignores the zero caller.
	var caller stsCaller
	if !anonymousSTSActions[action] {
		caller, err = gw.resolveSTSCaller(r)
		if err != nil {
			return err
		}
	}

	// The identity policy is the first of two gates; the handler still applies
	// the role's trust policy. Scoping to the target role lets a policy grant
	// assumption of one role without granting all of them.
	if stsPolicyGatedActions[action] {
		resource, rerr := gw.stsPolicyResource(queryArgs)
		if rerr != nil {
			return rerr
		}
		if err := gw.checkPolicyResources(r, "sts", action, []string{resource}); err != nil {
			if denial, ok := errors.AsType[*identityPolicyDenialError](err); ok {
				return denial.detailedError()
			}
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
		return spxarn.FormatIAMRoot(accountID), nil
	case principalTypeUser:
		if identity == "root" && accountID == utils.GlobalAccountID {
			return spxarn.FormatIAMRoot(accountID), nil
		}
		if identity == "" {
			slog.Error("STS_Request: user principal without identity")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return spxarn.FormatIAMPath(spxarn.IAMUser, accountID, "/", identity), nil
	default:
		slog.Error("STS_Request: unknown principal type", "principalType", principalType)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}
