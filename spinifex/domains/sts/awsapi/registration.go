package awsapi

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsxml "github.com/mulgadc/spinifex/spinifex/foundation/aws/xml"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
)

// ServiceName is the SigV4 credential-scope service STS clients sign for.
const ServiceName = "sts"

const principalTypeAssumedRole = stsdomain.PrincipalTypeAssumedRole

// Deps are the composed capabilities the STS control plane dispatches to. A
// nil STS answers every action with InternalError.
type Deps struct {
	STS stsdomain.STSService
	// IAM resolves the target role of AssumeRole and the caller of
	// GetCallerIdentity.
	IAM iamdomain.IAMService
}

// caller is the SigV4-derived identity an action may consume, read once from
// the dispatch actor.
type caller struct {
	accountID     string
	arn           string
	identity      string
	principalType string
	assumedRoleID string
	accessKey     string
}

// action parses the request into a typed input and runs its handler.
type action struct {
	// anonymous actions authenticate by a token in the body, so no caller is
	// resolved for them.
	anonymous bool
	// gated actions need a pass on the caller's identity policy.
	gated bool
	run   func(name string, q map[string]string, c caller, d Deps) ([]byte, error)
}

// handler creates a type-safe action: it allocates the input struct, parses the
// query parameters, runs the handler and marshals its output to XML.
func handler[In any](anonymous, gated bool, h func(c caller, input *In, d Deps) (any, error)) action {
	return action{
		anonymous: anonymous,
		gated:     gated,
		run: func(name string, q map[string]string, c caller, d Deps) ([]byte, error) {
			input := new(In)
			if err := query.QueryParamsToStruct(q, input); err != nil {
				if errors.Is(err, query.ErrSliceTooLarge) {
					return nil, errors.New(awserrors.ErrorMalformedQueryString)
				}
				return nil, errors.New(awserrors.ErrorValidationError)
			}
			output, err := h(c, input, d)
			if err != nil {
				return nil, err
			}
			xmlOutput, err := awsxml.Marshal(awsxml.QueryResponsePayload(name, output))
			if err != nil {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			return xmlOutput, nil
		},
	}
}

var actions = map[string]action{
	"AssumeRole": handler(false, true, func(c caller, input *sts.AssumeRoleInput, d Deps) (any, error) {
		return AssumeRole(c.accountID, c.arn, c.identity, input, d.STS)
	}),
	"AssumeRoleWithWebIdentity": handler(true, false, func(_ caller, input *sts.AssumeRoleWithWebIdentityInput, d Deps) (any, error) {
		return AssumeRoleWithWebIdentity(input, d.STS)
	}),
	"GetAccessKeyInfo": handler(false, false, func(_ caller, input *sts.GetAccessKeyInfoInput, d Deps) (any, error) {
		return GetAccessKeyInfo(input, d.STS)
	}),
	"GetCallerIdentity": handler(false, false, func(c caller, input *sts.GetCallerIdentityInput, d Deps) (any, error) {
		return GetCallerIdentity(c.accountID, c.arn, c.principalType, c.identity, c.assumedRoleID, input, d.IAM, d.STS)
	}),
	"GetSessionToken": handler(false, false, func(c caller, input *sts.GetSessionTokenInput, d Deps) (any, error) {
		return GetSessionToken(c.accountID, c.identity, c.principalType, c.accessKey, input, d.STS)
	}),
}

// NewRegistration returns the STS AWS Query control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Traits:        dispatch.ServiceTraits{XMLThrottleCode: awserrors.ErrorThrottling},
		Inventory:     OperationInventory(),
	}
}

// ActionNames lists every action the registration dispatches, sorted.
func ActionNames() []string {
	return slices.Sorted(maps.Keys(actions))
}

// OperationInventory classifies the actions the registration serves.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{Registered: ActionNames()}
}

// AnonymousFunc runs one unsigned STS action from its parsed query arguments
// and answers the XML response body.
type AnonymousFunc func(queryArgs map[string]string) ([]byte, error)

// NewAnonymousAssumeRoleWithWebIdentity serves AssumeRoleWithWebIdentity for a
// request that carries no SigV4 envelope. The web-identity token in the body is
// the only authentication, so it runs no caller or identity-policy step.
func NewAnonymousAssumeRoleWithWebIdentity(deps Deps) AnonymousFunc {
	return func(q map[string]string) ([]byte, error) {
		if deps.STS == nil {
			slog.Error("STS: service not initialized")
			return nil, errors.New(awserrors.ErrorInternalError)
		}
		return actions["AssumeRoleWithWebIdentity"].run("AssumeRoleWithWebIdentity", q, caller{}, deps)
	}
}

// anonymousActions lists the STS actions that carry no SigV4; a web-identity
// token in the body authenticates them.
var anonymousActions = map[string]bool{
	"AssumeRoleWithWebIdentity": true,
}

// NewPreAuth serves the unsigned web-identity exchange ahead of SigV4. A
// request with an Authorization header, a body over the payload cap or any
// other action is left to the signed chain.
func NewPreAuth(deps Deps) dispatch.PreAuth {
	run := NewAnonymousAssumeRoleWithWebIdentity(deps)
	return dispatch.PreAuth{
		Service: ServiceName,
		Serve: func(w http.ResponseWriter, r *http.Request) (bool, error) {
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
				return false, nil
			}
			args, ok := anonymousArgs(r)
			if !ok {
				return false, nil
			}
			xmlOutput, err := run(args)
			if err != nil {
				return true, err
			}
			w.Header().Set("Content-Type", "text/xml")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write(xmlOutput); err != nil {
				slog.Error("Failed to write STS response", "err", err)
			}
			return true, nil
		},
	}
}

// anonymousArgs peeks the request body and reports whether its Action is one
// served without SigV4. This runs ahead of authentication, so an unauthenticated
// client controls the allocation: one byte past the cap detects an oversize body,
// which is handed back unread rather than dispatched.
func anonymousArgs(r *http.Request) (map[string]string, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, false
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	args, err := query.ParseArgs(string(body))
	if err != nil || !anonymousActions[args["Action"]] {
		return nil, false
	}
	return args, true
}

// resolveAction is the one Query Action parser telemetry and dispatch share.
// It leaves the body readable for the dispatcher and answers "" on any failure.
func resolveAction(r *http.Request) string {
	args, err := readQueryArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

func readQueryArgs(r *http.Request) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return query.ParseArgs(string(body))
}

// dispatch parses the request, resolves the caller, applies the identity-policy
// gate and runs the action. Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("STS: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	name := queryArgs["Action"]
	if name == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	h, ok := actions[name]
	if !ok {
		slog.Debug("STS: unknown action", "action", name)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if d.STS == nil {
		slog.Error("STS: service not initialized")
		return errors.New(awserrors.ErrorInternalError)
	}

	// An anonymous action carries no SigV4 envelope; its handler ignores the
	// zero caller.
	var c caller
	var actor dispatch.Actor
	if !h.anonymous {
		if c, actor, err = resolveCaller(inv); err != nil {
			return err
		}
	}

	// The identity policy is the first of two gates; the handler still applies
	// the role's trust policy. Scoping to the target role lets a policy grant
	// assumption of one role without granting all of them.
	if h.gated {
		resource, err := d.policyResource(queryArgs)
		if err != nil {
			return err
		}
		if err := inv.Authorize(ServiceName, name, []string{resource}, nil); err != nil {
			if denial, ok := errors.AsType[*dispatch.PolicyDenialError](err); ok {
				return detailedDenial(actor, denial)
			}
			return err
		}
	}

	xmlOutput, err := h.run(name, queryArgs, c, d)
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

// resolveCaller assembles the caller fields from the dispatch actor. The actor
// is populated even when no caller ARN can be composed.
func resolveCaller(inv dispatch.Invocation) (caller, dispatch.Actor, error) {
	actor, err := inv.Actor()
	if actor.AccountID == "" {
		slog.Error("STS_Request: no account ID in auth context")
		return caller{}, actor, errors.New(awserrors.ErrorInternalError)
	}
	if err != nil {
		return caller{}, actor, err
	}
	c := caller{
		accountID:     actor.AccountID,
		arn:           actor.CallerARN,
		identity:      actor.UserName,
		principalType: actor.PrincipalType,
		accessKey:     actor.AccessKeyID,
	}
	if actor.PrincipalType == principalTypeAssumedRole {
		c.identity = actor.SessionName
		c.assumedRoleID = actor.UserID
	}
	return c, actor, nil
}

// detailedDenial names the caller, action and resource. The shared
// authorization failure is opaque, and STS opts into detail because its
// resource is a role ARN the caller supplied.
func detailedDenial(actor dispatch.Actor, denial *dispatch.PolicyDenialError) error {
	callerARN := actor.CallerARN
	if callerARN == "" {
		callerARN = actor.UserName
		if actor.PrincipalType == principalTypeAssumedRole {
			callerARN = actor.SessionName
		}
	}
	return awserrors.Errorf(awserrors.ErrorAccessDenied,
		"User: %s is not authorized to perform: %s on resource: %s",
		callerARN, denial.Action, denial.Resource)
}

// policyResource returns the ARN a gated action is evaluated against.
// AssumeRole resolves the target role so the resource is the ARN IAM stored,
// which a policy naming a pathed role matches and an invented path does not.
func (d Deps) policyResource(queryArgs map[string]string) (string, error) {
	// An absent RoleArn is a malformed request, not an unauthorized one: saying
	// so beats sending the caller to widen a policy that was never the cause.
	roleARN := queryArgs["RoleArn"]
	if roleARN == "" {
		return "", errors.New(awserrors.ErrorMissingParameter)
	}
	if d.IAM == nil {
		slog.Error("STS: IAM service not available", "action", "AssumeRole")
		return "", errors.New(awserrors.ErrorInternalError)
	}

	_, role, err := stsdomain.ResolveRoleByARN(d.IAM, roleARN)
	switch {
	case err == nil:
		return aws.StringValue(role.Arn), nil
	case errors.Is(err, stsdomain.ErrRoleUnresolved):
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
