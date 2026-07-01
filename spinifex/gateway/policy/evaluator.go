// Package policy implements IAM policy evaluation for access control decisions.
package policy

import (
	"github.com/mulgadc/predastore/pkg/iampolicy"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// Decision represents the outcome of a policy evaluation. It aliases the shared
// predastore decision type so existing spinifex call sites are unchanged.
type Decision = iampolicy.Decision

const (
	// Deny is the default — no matching Allow, or an explicit Deny.
	Deny = iampolicy.Deny
	// Allow means an explicit Allow was found with no overriding Deny.
	Allow = iampolicy.Allow
)

// EvaluateAccess checks whether the given action is permitted on the specified
// resource by the supplied policy documents, following AWS's explicit-deny-wins
// evaluation order. It delegates to the shared iampolicy evaluator. The identity
// argument is retained for call-site compatibility (it was only ever used in a
// warn log) and is no longer consulted. Root bypass is handled by the gateway
// before this function is called.
func EvaluateAccess(_ string, action, resource string, policies []handlers_iam.PolicyDocument) Decision {
	return iampolicy.Evaluate(action, resource, policies)
}
