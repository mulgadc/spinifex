// Package policy implements IAM policy evaluation for access control decisions.
package policy

import (
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// Decision represents the outcome of a policy evaluation.
type Decision int

const (
	// Deny is the default — no matching Allow, or an explicit Deny.
	Deny Decision = iota
	// Allow means an explicit Allow was found with no overriding Deny.
	Allow
)

// EvaluateAccess checks whether the given identity is permitted to perform
// the specified action on the specified resource, based on the supplied
// policy documents. It follows AWS's evaluation order:
//
//  1. Explicit Deny in any statement → Deny (wins immediately).
//  2. Explicit Allow in any statement → Allow.
//  3. No matching statement → Deny (implicit default).
//
// Root bypass is handled by the gateway before this function is called.
func EvaluateAccess(identity, action, resource string, policies []handlers_iam.PolicyDocument) Decision {
	hasAllow := false
	for i := range policies {
		for j := range policies[i].Statement {
			stmt := &policies[i].Statement[j] //nolint:gosec // G602 false positive: j is bounded by range

			if !matchesAny(stmt.Action, action) {
				continue
			}
			if !matchesAny(stmt.Resource, resource) {
				continue
			}
			switch stmt.Effect {
			case handlers_iam.PolicyEffectDeny:
				return Deny
			case handlers_iam.PolicyEffectAllow:
				hasAllow = true
			default:
				slog.Warn("EvaluateAccess: unrecognized Effect, treating as Deny",
					"effect", stmt.Effect, "action", action, "identity", identity)
				return Deny
			}
		}
	}

	if hasAllow {
		return Allow
	}
	return Deny
}

// matchesAny reports whether any pattern matches value using AWS IAM wildcard
// semantics. Comparison is case-insensitive.
func matchesAny(patterns []string, value string) bool {
	lv := strings.ToLower(value)
	for _, p := range patterns {
		if filterutil.MatchWildcard(strings.ToLower(p), lv) {
			return true
		}
	}
	return false
}
