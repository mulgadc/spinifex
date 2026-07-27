package handlers_sts

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
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

	// issuer is the literal `iss` claim; used to build condition-key prefixes like `{iss}:sub`.
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
	doc, err := handlers_iam.ValidateTrustPolicyDocument(docJSON)
	if err != nil {
		return fmt.Errorf("stored trust policy invalid: %w", err)
	}

	for _, stmt := range doc.Statement {
		if stmt.Effect != handlers_iam.PolicyEffectDeny {
			continue
		}
		matched, err := matchWebIdentityStatement(stmt.Action, stmt.Principal, stmt.Condition, identity)
		if err != nil {
			return err
		}
		if matched {
			return errors.New(awserrors.ErrorAccessDenied)
		}
	}

	for _, stmt := range doc.Statement {
		if stmt.Effect != handlers_iam.PolicyEffectAllow {
			continue
		}
		matched, err := matchWebIdentityStatement(stmt.Action, stmt.Principal, stmt.Condition, identity)
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
	}

	return errors.New(awserrors.ErrorAccessDenied)
}

func matchWebIdentityStatement(actions []string, principalRaw, conditionRaw json.RawMessage, identity webIdentityContext) (bool, error) {
	if !matchWebIdentityAction(actions) {
		return false, nil
	}
	principalMatch, err := matchFederatedPrincipal(principalRaw, identity.federatedPrincipalARN)
	if err != nil {
		return false, err
	}
	if !principalMatch {
		return false, nil
	}
	if !conditionsHold(conditionRaw, identity) {
		return false, nil
	}
	return true, nil
}

func matchWebIdentityAction(actions []string) bool {
	for _, a := range actions {
		if a == stsActionAssumeRoleWithWebIdentity || a == stsActionWildcard || a == globalWildcard {
			return true
		}
	}
	return false
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

// conditionsHold evaluates a Condition block. Only StringEquals is supported;
// keys are {iss}:sub (equality) and {iss}:aud (set-membership). No Condition = grant.
func conditionsHold(raw json.RawMessage, identity webIdentityContext) bool {
	if !isCondRawNonEmpty(raw) {
		return true
	}
	var ops map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ops); err != nil {
		return false
	}
	for op, kv := range ops {
		if op != "StringEquals" {
			// Validator rejects other operators at write time; a non-StringEquals here means tampering.
			return false
		}
		issPrefix := strings.TrimSuffix(identity.issuer, "/")
		for key, expectedRaw := range kv {
			expected, ok := unmarshalStringOrArray(expectedRaw)
			if !ok {
				return false
			}
			switch key {
			case issPrefix + ":sub":
				if !anyEquals(expected, []string{identity.subject}) {
					return false
				}
			case issPrefix + ":aud":
				if !anyEquals(expected, identity.audience) {
					return false
				}
			default:
				// Unknown condition key — fail closed rather than silently ignore a stricter check.
				return false
			}
		}
	}
	return true
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
