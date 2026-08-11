package gateway

import (
	"context"
	"net/http"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// allowAllIAMService resolves an allow-everything policy for every principal.
// Tests that exercise handler behaviour rather than authorization use it so the
// policy gate passes on a real evaluation instead of a bypass.
func allowAllIAMService() *policyMockIAMService {
	allow := []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Allow", Action: handlers_iam.StringOrArr{"*"}, Resource: handlers_iam.StringOrArr{"*"}},
		},
	}}
	return &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return allow, nil },
		getRolePoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return allow, nil },
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
