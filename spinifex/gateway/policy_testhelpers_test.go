package gateway

import (
	"context"
	"net/http"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// allowAllIAMService resolves an allow-everything policy for every principal.
// Tests that exercise handler behaviour rather than authorization use it so the
// policy gate passes on a real evaluation instead of a bypass.
func allowAllIAMService() *policyMockIAMService {
	allow := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
	return &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
		getRolePoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
	}
}

// withTestIdentity attaches the identity half of the auth context the SigV4
// middleware sets, so a hand-built request reaches the handler instead of being
// denied. Existing values are preserved for tests that set their own principal.
func withTestIdentity(req *http.Request) *http.Request {
	ctx := req.Context()
	if ctx.Value(ctxIdentity) == nil {
		ctx = context.WithValue(ctx, ctxIdentity, "alice")
	}
	if ctx.Value(ctxPrincipalType) == nil {
		ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	}
	return req.WithContext(ctx)
}
