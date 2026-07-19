package gateway

import (
	"fmt"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	"github.com/mulgadc/spinifex/spinifex/gateway/policy"
)

// ecrOperationAuthorization runs after ecrAuthBridge has rehydrated and
// stashed the current principal: it classifies the request's OCI operation,
// builds the repository ARN(s) the route requires, and evaluates every
// required ecr: action through the same identity-policy evaluator SigV4
// requests use. The request only reaches Registry once every requirement
// evaluates allow — a token that authenticates a real but under-permissioned
// principal (e.g. AmazonEC2ContainerRegistryPullOnly) must not push, delete,
// or overwrite despite carrying a validly signed token.
//
// A nil ECRTokenVerifier disables ecrAuthBridge upstream (registry mounts
// open in unit tests of unrelated routes); this middleware mirrors that
// bypass so it never blocks those tests.
func (gw *GatewayConfig) ecrOperationAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gw.ECRTokenVerifier == nil {
			next.ServeHTTP(w, r)
			return
		}

		principal, ok := r.Context().Value(ctxECRPrincipal).(principalContext)
		if !ok {
			// ecrAuthBridge did not stash a principal: it either already rejected
			// the request (this handler is never reached) or is disabled in a way
			// that skipped rehydration. Fail closed rather than dispatch with no
			// principal to evaluate.
			gateway_ecr.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}

		op, ok := gateway_ecr.ClassifyOperation(r.Method, r.URL.Path, r.URL.Query())
		if !ok {
			// Registry itself would 404 this path/method combination; refusing it
			// here too means an operation added to Registry without a matching
			// classifier case can never dispatch unauthorized.
			gateway_ecr.WriteError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognized registry operation")
			return
		}

		for _, req := range op.Requirements {
			resource, err := ecrResourceARN(gw.Region, principal.accountID, req.Scope, op)
			if err != nil {
				gateway_ecr.WriteError(w, http.StatusServiceUnavailable, "UNKNOWN", "authorization unavailable")
				return
			}
			if err := gw.evaluatePrincipalPolicy(principal, policy.IAMAction("ecr", req.Action), resource); err != nil {
				if err.Error() == awserrors.ErrorInternalError {
					// IAM/STS/NATS dependency failure: fail closed, never dispatch.
					gateway_ecr.WriteError(w, http.StatusServiceUnavailable, "UNKNOWN", "authorization unavailable")
					return
				}
				gateway_ecr.WriteError(w, http.StatusForbidden, "DENIED", "insufficient permissions")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// ecrResourceARN builds the repository ARN an ActionRequirement's scope
// resolves to, from the gateway's configured region and the token's account.
func ecrResourceARN(region, accountID string, scope gateway_ecr.ResourceScope, op gateway_ecr.ClassifiedOperation) (string, error) {
	switch scope {
	case gateway_ecr.ScopeAccountWildcard:
		return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/*", region, accountID), nil
	case gateway_ecr.ScopeDestination:
		return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/%s", region, accountID, op.Repo), nil
	case gateway_ecr.ScopeSource:
		return fmt.Sprintf("arn:aws:ecr:%s:%s:repository/%s", region, accountID, op.Source), nil
	default:
		return "", fmt.Errorf("unhandled ECR resource scope %d", scope)
	}
}
