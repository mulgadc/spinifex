// Package policy implements IAM policy evaluation for access control decisions.
package policy

import (
	"log/slog"
	"strings"

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

// matchesAny returns true if any pattern in patterns matches the given value.
// Supports the same wildcard matching for both actions and resources:
//   - "*"                — matches everything
//   - "ec2:*"            — matches all actions in the ec2 service
//   - "s3:Get*"          — matches s3:GetObject, s3:GetBucketPolicy, etc.
//   - "ec2:RunInstances" — exact match
func matchesAny(patterns []string, value string) bool {
	for _, p := range patterns {
		if matchWildcard(p, value) {
			return true
		}
	}
	return false
}

// matchWildcard performs AWS IAM-style wildcard matching where "*" matches
// zero or more characters at any position in the pattern. Matching is
// case-insensitive, matching the convention used for action names and
// extended to resource ARNs for our internal use.
//
// Examples:
//
//	"*"                              matches anything
//	"ec2:*"                          matches "ec2:RunInstances"
//	"s3:Get*"                        matches "s3:GetObject"
//	"arn:aws:iam::*:role/app-*"      matches "arn:aws:iam::123456789012:role/app-foo"
//	"ec2:RunInstances"               matches only "ec2:RunInstances"
func matchWildcard(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, value)
	}

	lp := strings.ToLower(pattern)
	lv := strings.ToLower(value)
	parts := strings.Split(lp, "*")
	last := len(parts) - 1

	if !strings.HasPrefix(lv, parts[0]) {
		return false
	}
	if !strings.HasSuffix(lv, parts[last]) {
		return false
	}

	remaining := lv[len(parts[0]):]
	if len(remaining) < len(parts[last]) {
		return false
	}
	remaining = remaining[:len(remaining)-len(parts[last])]

	for i := 1; i < last; i++ {
		idx := strings.Index(remaining, parts[i])
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(parts[i]):]
	}
	return true
}
