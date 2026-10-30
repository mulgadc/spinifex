// Package policyauth evaluates a caller's identity policies for the AWS ingress. The
// SigV4 gateway path and the ECR registry both authorize through it, so the policy
// snapshot, the root bypass and the condition keys have one implementation.
package policyauth

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
	"github.com/nats-io/nats.go"
)

// defaultRetryBase scales the backoff between policy-resolve attempts after a
// transient NATS error.
const defaultRetryBase = 200 * time.Millisecond

// Evaluator evaluates identity policies held by the IAM service.
type Evaluator struct {
	iam       iamdomain.IAMService
	retryBase time.Duration
}

// Option configures an Evaluator.
type Option func(*Evaluator)

// WithRetryBase sets the backoff step between policy-resolve attempts.
func WithRetryBase(d time.Duration) Option { return func(e *Evaluator) { e.retryBase = d } }

// New returns an Evaluator. A nil iam fails every evaluation closed.
func New(iam iamdomain.IAMService, opts ...Option) *Evaluator {
	e := &Evaluator{iam: iam, retryBase: defaultRetryBase}
	for _, o := range opts {
		o(e)
	}
	return e
}

// DenialError is an identity-policy denial. It answers AccessDenied and, for a service
// whose resource is safe to name, unwraps to a dispatch.PolicyDenialError.
type DenialError struct {
	Principal   credential.Principal
	LogIdentity string
	Action      string
	Resource    string
}

func (d *DenialError) Error() string { return awserrors.ErrorAccessDenied }

func (d *DenialError) Unwrap() error {
	return &dispatch.PolicyDenialError{Action: d.Action, Resource: d.Resource}
}

// Evaluate resolves the principal's policies once and evaluates every resource against
// that snapshot, returning a *DenialError on the first Deny.
func (e *Evaluator) Evaluate(
	principal credential.Principal, iamAction string, resources []string, keys iampolicy.ConditionKeys,
) error {
	if e == nil || e.iam == nil {
		slog.Error("evaluatePrincipalPolicy: IAM service not available", "action", iamAction)
		return errors.New(awserrors.ErrorInternalError)
	}

	// Each branch resolves the policy resolver and log identity for its principal
	// type. Identity-sensitive decisions (root bypass, resolver selection) are
	// fully inside their branch so an assumed-role SessionName of "root" cannot
	// reach the user-branch root short-circuit.
	var resolve func() ([]iamdomain.PolicyDocument, error)
	var logIdentity string

	switch principal.PrincipalType {
	case stsdomain.PrincipalTypeUser:
		if principal.Identity == "root" && principal.AccountID == awsidentifiers.GlobalAccountID {
			// Global root bypass — user branch only.
			return nil
		}
		resolve = func() ([]iamdomain.PolicyDocument, error) {
			return e.iam.GetUserPolicies(principal.AccountID, principal.Identity)
		}
		logIdentity = principal.Identity
	case stsdomain.PrincipalTypeAssumedRole:
		// Resolve by the session's underlying role, never by SessionName (attacker-influenced).
		// A missing/legacy, cross-account, or malformed ARN fails closed with AccessDenied.
		roleAcct, roleName, perr := auth.ParseRoleARN(principal.UnderlyingRoleARN)
		if perr != nil || roleAcct != principal.AccountID {
			slog.Warn("evaluatePrincipalPolicy: unresolvable or cross-account assumed-role principal denied",
				"underlyingRoleARN", principal.UnderlyingRoleARN,
				"accountID", principal.AccountID,
				"action", iamAction,
				"err", perr)
			return errors.New(awserrors.ErrorAccessDenied)
		}
		resolve = func() ([]iamdomain.PolicyDocument, error) {
			return e.iam.GetRolePolicies(principal.AccountID, roleName)
		}
		logIdentity = principal.AssumedRoleARN
	default:
		slog.Error("evaluatePrincipalPolicy: unknown principal type", "principalType", principal.PrincipalType)
		return errors.New(awserrors.ErrorInternalError)
	}

	// Resolve policies, retrying transient NATS errors. Fail-closed on non-transient errors.
	var policies []iamdomain.PolicyDocument
	var err error
	for attempt := range 3 {
		policies, err = resolve()
		if err == nil || !isNATSTransient(err) {
			break
		}
		if attempt < 2 {
			slog.Debug("evaluatePrincipalPolicy: transient NATS error, retrying",
				"identity", logIdentity, "attempt", attempt+1, "err", err)
			time.Sleep(time.Duration(attempt+1) * e.retryBase)
		}
	}
	if err != nil {
		slog.Error("evaluatePrincipalPolicy: failed to resolve policies", "identity", logIdentity, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	for _, resource := range resources {
		if iampolicy.EvaluateWithKeys(iamAction, resource, policies, keys) == iampolicy.Deny {
			slog.Info("evaluatePrincipalPolicy: access denied",
				"identity", logIdentity, "action", iamAction, "resource", resource)
			return &DenialError{Principal: principal, LogIdentity: logIdentity, Action: iamAction, Resource: resource}
		}
	}
	return nil
}

// isNATSTransient reports whether err is a transient NATS/JetStream failure that may
// resolve after cluster leader election completes.
func isNATSTransient(err error) bool {
	return err != nil && (errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoStreamResponse))
}

// RequestConditionKeys resolves the IAM condition context keys available on the AWS API
// path. s3:prefix has no meaning here and is deliberately absent, so a policy
// conditioned on it does not fire.
//
// Every key is omitted rather than set empty when unknown: an empty value reads as a
// real value that matches nothing, and on a Deny that silently widens access instead
// of narrowing it.
func RequestConditionKeys(r *http.Request, principal credential.Principal) iampolicy.ConditionKeys {
	keys := iampolicy.ConditionKeys{
		iampolicy.KeySecureTransport: strconv.FormatBool(r.TLS != nil),
	}
	// aws:username is user-only in AWS. A role session's identity is the
	// caller-chosen RoleSessionName, so gating authorization on it here would
	// let any principal that may assume the role satisfy the condition at will.
	if principal.PrincipalType == stsdomain.PrincipalTypeUser && principal.Identity != "" {
		keys[iampolicy.KeyUsername] = principal.Identity
	}
	if principal.AccountID != "" {
		keys[iampolicy.KeyPrincipalAccount] = principal.AccountID
	}
	// Resolved once where the principal was, so a per-check lookup cannot leave
	// one check in a request evaluating against a different context than the next.
	if principal.UserID != "" {
		keys[iampolicy.KeyUserID] = principal.UserID
	}
	// Derived from the request rather than read from the context: the OCI
	// registry chain never runs SigV4AuthMiddleware, so a context-carried
	// address would be absent there and every aws:SourceIp condition inert.
	if ip := ingresshttp.ClientIP(r.RemoteAddr); ip != "" {
		keys[iampolicy.KeySourceIP] = ip
	}
	// Resolved from the credential record, never from anything the caller
	// supplies, so unlike aws:username it is safe for a role session too.
	if v, ok := principalTypeCondition(principal.PrincipalType); ok {
		keys[iampolicy.KeyPrincipalType] = v
	}
	// The server clock, never a request header, as aws:CurrentTime is in AWS.
	keys.SetRequestTime(time.Now())
	return keys
}

// principalTypeCondition maps the internal principal-type spelling to the canonical
// aws:PrincipalType value AWS documents, so a policy written against the documented
// value applies here and at predastore identically. An empty or unrecognized type
// reports false so the door omits the key rather than supplying an empty string.
func principalTypeCondition(principalType string) (string, bool) {
	switch principalType {
	case stsdomain.PrincipalTypeUser:
		return iampolicy.PrincipalTypeUser, true
	case stsdomain.PrincipalTypeAssumedRole:
		return iampolicy.PrincipalTypeAssumedRole, true
	case stsdomain.PrincipalTypeRoot:
		return iampolicy.PrincipalTypeAccount, true
	default:
		return "", false
	}
}
