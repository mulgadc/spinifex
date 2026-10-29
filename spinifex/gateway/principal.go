package gateway

import (
	"context"
	"net/http"
)

// callerPrincipal is the verified caller, sealed once per request by the SigV4
// middleware. It holds only strings, so a copy cannot change what the next
// reader sees and every policy check in a request evaluates the same caller.
type callerPrincipal struct {
	// identity is the IAM user name for a user principal and the role session
	// name for an assumed-role one. Gate on principalType before reading it as
	// a user name.
	identity    string
	accountID   string
	accessKeyID string

	principalType  string
	assumedRoleARN string
	assumedRoleID  string
	// underlyingRoleARN is the IAM role behind an assumed-role session. The role
	// name comes from here, never from identity, which the caller chose.
	underlyingRoleARN string
	// userID is aws:userid, resolved where the principal is.
	userID string
	// userARN is an IAM user's stored ARN, path included. Empty for other types.
	userARN string
}

// ctxPrincipal is the single request-context key holding the callerPrincipal.
const ctxPrincipal contextKey = "sigv4.principal"

// withCallerPrincipal seals p into ctx. Only the SigV4 middleware calls it.
func withCallerPrincipal(ctx context.Context, p callerPrincipal) context.Context {
	return context.WithValue(ctx, ctxPrincipal, p)
}

// callerPrincipalFrom returns the sealed caller, or false when the request was
// not authenticated.
func callerPrincipalFrom(ctx context.Context) (callerPrincipal, bool) {
	p, ok := ctx.Value(ctxPrincipal).(callerPrincipal)
	return p, ok
}

// callerPrincipalOf returns the sealed caller, or the zero value when the
// request was not authenticated.
func callerPrincipalOf(r *http.Request) callerPrincipal {
	p, _ := callerPrincipalFrom(r.Context())
	return p
}

// policyPrincipal is the caller as the policy evaluator takes it.
func (p callerPrincipal) policyPrincipal() principalContext {
	return principalContext{
		identity:          p.identity,
		accountID:         p.accountID,
		principalType:     p.principalType,
		assumedRoleARN:    p.assumedRoleARN,
		assumedRoleID:     p.assumedRoleID,
		underlyingRoleARN: p.underlyingRoleARN,
		userID:            p.userID,
		userARN:           p.userARN,
	}
}
