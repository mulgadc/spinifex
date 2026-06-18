package gateway_ecr

import "context"

// ctxKey is a private context key type for this package.
type ctxKey string

// ctxAuthAccount carries the accountID resolved from a verified ECR token by the
// /v2 auth bridge. The Registry scopes per-request storage to it.
const ctxAuthAccount ctxKey = "ecr.authAccount"

// WithAuthAccount returns a context carrying the auth-bridge–resolved account.
// The gateway auth bridge calls this after verifying a registry token.
func WithAuthAccount(ctx context.Context, accountID string) context.Context {
	return context.WithValue(ctx, ctxAuthAccount, accountID)
}

// authAccount returns the auth-bridge–resolved account from ctx, if any.
func authAccount(ctx context.Context) string {
	a, _ := ctx.Value(ctxAuthAccount).(string)
	return a
}
