package sts

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
)

const (
	// stsActionAssumeRoleWithWebIdentity is the only action that may carry a Condition block.
	stsActionAssumeRoleWithWebIdentity = "sts:AssumeRoleWithWebIdentity"

	// irsaExpectedAudience is the audience value IRSA pods bake into ServiceAccount tokens.
	irsaExpectedAudience = "sts.amazonaws.com"
)

// webIdentityContext holds the JWT-verified facts the trust-policy evaluator matches against.
type webIdentityContext struct {
	// federatedPrincipalARN is the oidc-provider ARN Principal.Federated must equal.
	federatedPrincipalARN string

	// issuer is the literal `iss` claim; its host-path (or scheme-prefixed form) prefixes condition keys like `{issuer}:sub`.
	issuer string

	// subject is the literal `sub` claim.
	subject string

	// audience holds the JWT `aud` values; a StringEquals condition matches if any entry equals the expected value.
	audience []string
}

// evalTrustPolicyForWebIdentity evaluates an AssumeRoleWithWebIdentity trust policy.
// Explicit-deny wins; a statement matches when Action, Principal.Federated, and all Conditions hold.
// Returns nil on grant or AccessDenied/error on deny/corruption.
func evalTrustPolicyForWebIdentity(docJSON string, identity webIdentityContext) error {
	doc, err := iamdomain.ValidateTrustPolicyDocument(docJSON)
	if err != nil {
		return fmt.Errorf("stored trust policy invalid: %w", err)
	}

	for _, stmt := range doc.Statement {
		if stmt.Effect != iamdomain.PolicyEffectDeny {
			continue
		}
		matched, _, err := matchWebIdentityStatement(stmt.Action, stmt.Principal, stmt.Condition, identity)
		if err != nil {
			return err
		}
		if matched {
			return &trustPolicyDeniedError{reason: denyReasonExplicitDeny}
		}
	}

	denial := &trustPolicyDeniedError{reason: denyReasonNoStatement}

	for _, stmt := range doc.Statement {
		if stmt.Effect != iamdomain.PolicyEffectAllow {
			continue
		}
		matched, cd, err := matchWebIdentityStatement(stmt.Action, stmt.Principal, stmt.Condition, identity)
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
		// Keep the first statement that matched action and principal but failed its condition.
		if cd != nil && denial.reason == denyReasonNoStatement {
			denial = &trustPolicyDeniedError{reason: denyReasonCondMismatch, operator: cd.operator, key: cd.key}
			if cd.unknownKey {
				denial.reason = denyReasonCondUnsupport
			}
		}
	}

	return denial
}

// matchWebIdentityStatement reports whether a statement applies; when only its Condition failed, the denial says why.
func matchWebIdentityStatement(actions []string, principalRaw, conditionRaw json.RawMessage, identity webIdentityContext) (bool, *condDenial, error) {
	if !matchWebIdentityAction(actions) {
		return false, nil, nil
	}
	principalMatch, err := matchFederatedPrincipal(principalRaw, identity.federatedPrincipalARN)
	if err != nil {
		return false, nil, err
	}
	if !principalMatch {
		return false, nil, nil
	}
	if held, cd := conditionsHold(conditionRaw, identity); !held {
		return false, &cd, nil
	}
	return true, nil, nil
}

func matchWebIdentityAction(actions []string) bool {
	return slices.ContainsFunc(actions, func(a string) bool {
		return a == stsActionAssumeRoleWithWebIdentity || a == stsActionWildcard || a == globalWildcard
	})
}

// matchFederatedPrincipal accepts Principal.Federated as a string or array.
// Returns false for Principal: "*" — Federated does not honour the bare wildcard.
// Unrelated top-level keys (Service, AWS) are skipped.
func matchFederatedPrincipal(raw json.RawMessage, expectedARN string) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		// Principal: "*" does not match Federated — fail closed to prevent any-issuer grants.
		return false, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, fmt.Errorf("unmarshal Principal: %w", err)
	}
	fedRaw, ok := m["Federated"]
	if !ok {
		return false, nil
	}
	var single string
	if err := json.Unmarshal(fedRaw, &single); err == nil {
		return subtle.ConstantTimeCompare([]byte(single), []byte(expectedARN)) == 1, nil
	}
	var arr []string
	if err := json.Unmarshal(fedRaw, &arr); err != nil {
		return false, fmt.Errorf("Principal.Federated must be string or array: %w", err)
	}
	for _, entry := range arr {
		if subtle.ConstantTimeCompare([]byte(entry), []byte(expectedARN)) == 1 {
			return true, nil
		}
	}
	return false, nil
}

// Reasons a trust policy denied the assumption, logged by the caller.
const (
	denyReasonNoStatement   = "no statement matched"
	denyReasonExplicitDeny  = "explicit deny"
	denyReasonCondMismatch  = "condition value mismatch"
	denyReasonCondUnsupport = "condition operator or key unsupported"
)

// trustPolicyDeniedError is the AccessDenied error carrying why, for logs only; the API response is unchanged.
type trustPolicyDeniedError struct {
	reason   string
	operator string
	key      string
}

func (d *trustPolicyDeniedError) Error() string { return awserrors.ErrorAccessDenied }

// condDenial says why a Condition block did not hold, for the caller to log.
type condDenial struct {
	operator string
	key      string
	// unknownKey is true when the key or operator is unsupported, false when the value did not match.
	unknownKey bool
}

// webIdentityHostPath is the issuer without scheme or trailing slash; it prefixes condition keys only.
func webIdentityHostPath(issuer string) string {
	return strings.TrimSuffix(strings.TrimPrefix(issuer, "https://"), "/")
}

// conditionsHold evaluates a Condition block: StringEquals and StringLike on {issuer}:sub and {issuer}:aud.
// The issuer is the host-path (AWS form) or the scheme-prefixed legacy form; no Condition = grant.
// Values under one key are OR'd; keys and operators are AND'd. Anything unrecognised fails closed.
func conditionsHold(raw json.RawMessage, identity webIdentityContext) (bool, condDenial) {
	if !isCondRawNonEmpty(raw) {
		return true, condDenial{}
	}
	var ops map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ops); err != nil {
		return false, condDenial{unknownKey: true}
	}
	hostPath := webIdentityHostPath(identity.issuer)
	subKeys := []string{hostPath + ":sub", "https://" + hostPath + ":sub"}
	audKeys := []string{hostPath + ":aud", "https://" + hostPath + ":aud"}
	for op, kv := range ops {
		var match func(want, got []string) bool
		switch op {
		case "StringEquals":
			match = anyEquals
		case "StringLike":
			match = anyLike
		default:
			// Validator rejects other operators at write time; one here means tampering.
			return false, condDenial{operator: op, unknownKey: true}
		}
		for key, expectedRaw := range kv {
			expected, ok := unmarshalStringOrArray(expectedRaw)
			if !ok {
				return false, condDenial{operator: op, key: key, unknownKey: true}
			}
			switch {
			case slices.Contains(subKeys, key):
				if !match(expected, []string{identity.subject}) {
					return false, condDenial{operator: op, key: key}
				}
			case slices.Contains(audKeys, key):
				if !match(expected, identity.audience) {
					return false, condDenial{operator: op, key: key}
				}
			default:
				// Unknown condition key — fail closed rather than silently ignore a stricter check.
				return false, condDenial{operator: op, key: key, unknownKey: true}
			}
		}
	}
	return true, condDenial{}
}

// isCondRawNonEmpty reports whether a Condition RawMessage has meaningful content.
func isCondRawNonEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "{}":
		return false
	default:
		return true
	}
}

func unmarshalStringOrArray(raw json.RawMessage) ([]string, bool) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, true
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, true
	}
	return nil, false
}

func anyEquals(want, got []string) bool {
	for _, w := range want {
		for _, g := range got {
			if subtle.ConstantTimeCompare([]byte(w), []byte(g)) == 1 {
				return true
			}
		}
	}
	return false
}

// anyLike reports whether any pattern in want matches any value in got, with IAM StringLike * and ? wildcards.
func anyLike(want, got []string) bool {
	for _, w := range want {
		for _, g := range got {
			if filters.MatchWildcard(w, g) {
				return true
			}
		}
	}
	return false
}
