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
	// stsActionAssumeRoleWithWebIdentity is the only Action whose statements
	// may carry a Condition block (per the validator in handlers_iam). The
	// constant is duplicated here so the action-match path stays in this
	// package and a future rename surfaces as a compile-time mismatch.
	stsActionAssumeRoleWithWebIdentity = "sts:AssumeRoleWithWebIdentity"

	// IRSA convention: pods bake a single audience value into the projected
	// ServiceAccount token. The handler accepts any token whose `aud` claim
	// contains this exact string (claim is a JSON string or array — handled
	// upstream via jwt.ClaimStrings).
	irsaExpectedAudience = "sts.amazonaws.com"
)

// webIdentityContext is the resolved-and-verified set of facts about an
// AssumeRoleWithWebIdentity caller that the trust-policy evaluator needs to
// match against. The handler builds it after JWT signature + claim validation;
// the evaluator does not re-verify, it only matches.
type webIdentityContext struct {
	// federatedPrincipalARN is `arn:aws:iam::{roleAccountID}:oidc-provider/{issuerHostPath}`
	// (the value the role trust policy's Principal.Federated entry must equal).
	federatedPrincipalARN string

	// issuer is the literal `iss` claim value. Used to build the condition-key
	// prefix `{iss}:sub` / `{iss}:aud` per the IRSA convention.
	issuer string

	// subject is the literal `sub` claim value.
	subject string

	// audience is the list of audience values from the JWT `aud` claim.
	// Membership semantics — a single Condition StringEquals on `{iss}:aud`
	// matches if any wired audience equals the condition value.
	audience []string
}

// evalTrustPolicyForWebIdentity is the sibling of evalTrustPolicy for the
// AssumeRoleWithWebIdentity action. AWS semantics: explicit-deny wins across
// the document, then any matching Allow grants. A statement matches when:
//
//   - Action contains sts:AssumeRoleWithWebIdentity (wildcards accepted —
//     sts:* and "*" — same as evalTrustPolicy for symmetry).
//   - Principal.Federated equals webIdentityContext.federatedPrincipalARN.
//   - Every Condition operator entry holds (currently only StringEquals).
//
// Returns nil on grant; awserrors.ErrorAccessDenied on no-match or explicit
// deny; a wrapped error on stored-doc corruption (matches evalTrustPolicy's
// fail-closed-loudly contract).
func evalTrustPolicyForWebIdentity(docJSON string, ctx webIdentityContext) error {
	doc, err := handlers_iam.ValidateTrustPolicyDocument(docJSON)
	if err != nil {
		return fmt.Errorf("stored trust policy invalid: %w", err)
	}

	for _, stmt := range doc.Statement {
		if stmt.Effect != handlers_iam.PolicyEffectDeny {
			continue
		}
		matched, err := matchWebIdentityStatement(stmt.Action, stmt.Principal, stmt.Condition, ctx)
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
		matched, err := matchWebIdentityStatement(stmt.Action, stmt.Principal, stmt.Condition, ctx)
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
	}

	return errors.New(awserrors.ErrorAccessDenied)
}

func matchWebIdentityStatement(actions []string, principalRaw, conditionRaw json.RawMessage, ctx webIdentityContext) (bool, error) {
	if !matchWebIdentityAction(actions) {
		return false, nil
	}
	principalMatch, err := matchFederatedPrincipal(principalRaw, ctx.federatedPrincipalARN)
	if err != nil {
		return false, err
	}
	if !principalMatch {
		return false, nil
	}
	if !conditionsHold(conditionRaw, ctx) {
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

// matchFederatedPrincipal accepts Principal.Federated as either a string or
// an array of strings, mirroring the AWS Principal.AWS shape. Returns false
// on Principal: "*" — Federated does not honour the bare wildcard the way
// Principal: "*" does; a web-identity policy that means "any OIDC issuer"
// must say so explicitly via a Principal.Federated entry per issuer ARN.
// Unrelated top-level keys (Service, AWS) skip at the entry level — same
// open-set semantics as matchTrustPrincipal in assume_role.go.
func matchFederatedPrincipal(raw json.RawMessage, expectedARN string) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		// Principal: "*" → does NOT match Federated. The web-identity action
		// has no analogue to the AWS-account-wide root principal and the bare
		// wildcard would silently grant any OIDC issuer that could fish a
		// trust policy. Fail closed.
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

// conditionsHold evaluates a Condition block against the web-identity context.
// The validator restricts the operator to StringEquals — anything else is a
// stored-doc inconsistency and the safe default is "do not match".
//
// IRSA-shaped keys:
//
//	{iss}:sub  → exact equality with the JWT `sub` claim
//	{iss}:aud  → set-membership against the JWT `aud` claim list
//	             (`aud` is a ClaimStrings — one or many)
//
// Each StringEquals entry value can be a single string or a list — both
// shapes match if any entry compares equal. Missing-condition = grant
// (no Condition → unrestricted Allow on the Federated principal).
func conditionsHold(raw json.RawMessage, ctx webIdentityContext) bool {
	if !isCondRawNonEmpty(raw) {
		return true
	}
	var ops map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ops); err != nil {
		return false
	}
	for op, kv := range ops {
		if op != "StringEquals" {
			// Validator rejects other operators at write time; reaching here
			// means a stored doc was tampered with bypassing validation. Fail
			// closed rather than silently grant.
			return false
		}
		issPrefix := strings.TrimSuffix(ctx.issuer, "/")
		for key, expectedRaw := range kv {
			expected, ok := unmarshalStringOrArray(expectedRaw)
			if !ok {
				return false
			}
			switch key {
			case issPrefix + ":sub":
				if !anyEquals(expected, []string{ctx.subject}) {
					return false
				}
			case issPrefix + ":aud":
				if !anyEquals(expected, ctx.audience) {
					return false
				}
			default:
				// Unknown condition key — fail closed. v1 only models the two
				// IRSA-shaped keys above; an unknown key in a stored doc most
				// likely means the policy author wanted a stricter check the
				// evaluator cannot honour, so refusing the grant is safer
				// than ignoring the key.
				return false
			}
		}
	}
	return true
}

// isCondRawNonEmpty mirrors the validator's empty-detection so an empty `{}`
// Condition (validator-accepted) does not enter the operator loop.
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
