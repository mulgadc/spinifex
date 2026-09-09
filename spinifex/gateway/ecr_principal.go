package gateway

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// An ECR registry token is a signed pointer to an IAM/STS record, not a
// permission grant: possession of a verified, unexpired JWT only proves the
// bearer once authenticated as the access key named in its claims. Every
// /v2/* request must resolve that access key against the *current* IAM/STS
// state before dispatch, so a key deactivation, session expiry/deletion, user
// or role deletion, account suspension, or policy change takes effect on the
// very next request — including one carrying an already-issued 12h token.
//
// resolveECRPrincipal performs that rehydration and returns the same
// principalContext the SigV4 middleware builds, so ECR requests are
// authorized by the identical policy-evaluation path as every other AWS
// action.

// ecrPrincipalError classifies a principal-rehydration failure so the HTTP
// layer can fail closed with the right status: an invalid/revoked/mismatched
// identity is client-visible (401, with the registry's re-auth challenge),
// while a lookup that could not complete is an operator-visible dependency
// failure (503) that must never be treated as an allow.
type ecrPrincipalError struct {
	dependency bool
	err        error
}

func (e *ecrPrincipalError) Error() string { return e.err.Error() }
func (e *ecrPrincipalError) Unwrap() error { return e.err }

// ecrInvalidPrincipal wraps a reason the presented identity pointer is no
// longer valid (revoked, expired, deleted, or inconsistent with the signed claims).
func ecrInvalidPrincipal(format string, args ...any) error {
	return &ecrPrincipalError{err: fmt.Errorf(format, args...)}
}

// ecrDependencyFailure wraps a reason the identity pointer could not be
// checked at all (IAM/STS/NATS unavailable). Never treat this as an allow.
func ecrDependencyFailure(format string, args ...any) error {
	return &ecrPrincipalError{dependency: true, err: fmt.Errorf(format, args...)}
}

// isECRDependencyFailure reports whether err (from resolveECRPrincipal or a
// caller further down the ECR chain) represents a backend outage rather than
// an invalid identity, so the HTTP layer can choose 503 over 401.
func isECRDependencyFailure(err error) bool {
	var pe *ecrPrincipalError
	return errors.As(err, &pe) && pe.dependency
}

// classifyIAMLookupErr maps an IAMService lookup error to the right
// ecrPrincipalError kind: a NoSuchEntity-shaped error means the referenced
// record is gone (invalid, 401); anything else is a dependency failure (503),
// since it says nothing about whether the identity is still valid.
func classifyIAMLookupErr(err error, what string) error {
	if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
		return ecrInvalidPrincipal("%s not found: %w", what, err)
	}
	return ecrDependencyFailure("%s lookup failed: %w", what, err)
}

// resolveECRPrincipal rehydrates claims (already ES256/expiry/issuer/audience
// verified by gateway_ecrauth.Verifier) against current IAM/STS state and
// returns the equivalent of a fresh SigV4 authentication for the same
// identity. It never trusts claims.PrincipalType or claims.Subject on their
// own — both are cross-checked against the freshly resolved record before
// being accepted, so a forged or stale claim cannot widen access beyond what
// the current IAM/STS state actually grants.
func (gw *GatewayConfig) resolveECRPrincipal(claims *gateway_ecrauth.Claims) (principalContext, error) {
	switch {
	case strings.HasPrefix(claims.AccessKeyID, longLivedAKIDPrefix):
		return gw.resolveECRLongLivedPrincipal(claims)
	case strings.HasPrefix(claims.AccessKeyID, sessionAKIDPrefix):
		return gw.resolveECRSessionPrincipal(claims)
	default:
		return principalContext{}, ecrInvalidPrincipal("unrecognized access key prefix")
	}
}

// resolveECRLongLivedPrincipal rehydrates an AKIA (IAM user) claim: the
// access key must still be active and belong to the claimed account, the
// user it names must still exist, the account must still be active, and the
// canonical ARN/principalType computed from that current state must match
// what was signed into the token.
func (gw *GatewayConfig) resolveECRLongLivedPrincipal(claims *gateway_ecrauth.Claims) (principalContext, error) {
	if gw.IAMService == nil {
		return principalContext{}, ecrDependencyFailure("IAM service not available")
	}

	ak, err := gw.IAMService.LookupAccessKey(claims.AccessKeyID)
	if err != nil {
		return principalContext{}, classifyIAMLookupErr(err, "access key")
	}
	if ak.Status != handlers_iam.AccessKeyStatusActive {
		return principalContext{}, ecrInvalidPrincipal("access key %q is inactive", claims.AccessKeyID)
	}
	if ak.AccountID != claims.AccountID {
		return principalContext{}, ecrInvalidPrincipal("access key account does not match token account")
	}

	if err := gw.requireActiveECRAccount(ak.AccountID); err != nil {
		return principalContext{}, err
	}

	userOut, err := gw.IAMService.GetUser(ak.AccountID, &iam.GetUserInput{UserName: aws.String(ak.UserName)})
	if err != nil {
		return principalContext{}, classifyIAMLookupErr(err, "user")
	}
	identity := aws.StringValue(userOut.User.UserName)

	if claims.PrincipalType != principalTypeUser {
		return principalContext{}, ecrInvalidPrincipal("principalType claim %q does not match resolved user", claims.PrincipalType)
	}
	canonicalARN, err := buildCallerARN(ak.AccountID, identity, principalTypeUser, "")
	if err != nil {
		return principalContext{}, ecrInvalidPrincipal("cannot build canonical ARN: %w", err)
	}
	if canonicalARN != claims.Subject {
		return principalContext{}, ecrInvalidPrincipal("subject claim does not match canonical principal ARN")
	}

	return principalContext{
		identity:      identity,
		accountID:     ak.AccountID,
		principalType: principalTypeUser,
		userID:        aws.StringValue(userOut.User.UserId),
	}, nil
}

// resolveECRSessionPrincipal rehydrates an ASIA (STS session) claim. Both a
// GetSessionToken user session and an assumed-role session are re-resolved
// against their principal's live IAM record and rejected if it was deleted, or
// recreated under the same name with a different immutable ID.
func (gw *GatewayConfig) resolveECRSessionPrincipal(claims *gateway_ecrauth.Claims) (principalContext, error) {
	if gw.STSService == nil {
		return principalContext{}, ecrDependencyFailure("STS service not available")
	}
	if gw.IAMService == nil {
		return principalContext{}, ecrDependencyFailure("IAM service not available")
	}

	cred, err := gw.STSService.LookupSessionCredential(claims.AccessKeyID)
	if err != nil {
		return principalContext{}, ecrDependencyFailure("session lookup failed: %w", err)
	}
	if cred == nil {
		return principalContext{}, ecrInvalidPrincipal("session %q not found", claims.AccessKeyID)
	}
	if time.Now().UTC().After(cred.ExpiresAt) {
		return principalContext{}, ecrInvalidPrincipal("session %q expired", claims.AccessKeyID)
	}
	if cred.AccountID != claims.AccountID {
		return principalContext{}, ecrInvalidPrincipal("session account does not match token account")
	}

	if err := gw.requireActiveECRAccount(cred.AccountID); err != nil {
		return principalContext{}, err
	}

	// One read of the live principal, shared by both branches: it both rejects a
	// session whose principal was deleted or recreated under the same name and
	// supplies the resolved record the branches below would otherwise re-read.
	live, err := gw.STSService.VerifySessionPrincipal(cred)
	if err != nil {
		if handlers_sts.IsSessionPrincipalVerdict(err) {
			return principalContext{}, ecrInvalidPrincipal("session principal is no longer valid: %w", err)
		}
		return principalContext{}, ecrDependencyFailure("session principal check failed: %w", err)
	}

	if cred.PrincipalType == principalTypeUser {
		// GetSessionToken: the session resolves to the same user identity as a
		// long-lived key, so it is authorized as that user.
		identity := cred.SessionName

		if claims.PrincipalType != principalTypeUser {
			return principalContext{}, ecrInvalidPrincipal("principalType claim %q does not match resolved session", claims.PrincipalType)
		}
		canonicalARN, err := buildCallerARN(cred.AccountID, identity, principalTypeUser, "")
		if err != nil {
			return principalContext{}, ecrInvalidPrincipal("cannot build canonical ARN: %w", err)
		}
		if canonicalARN != claims.Subject {
			return principalContext{}, ecrInvalidPrincipal("subject claim does not match canonical principal ARN")
		}

		return principalContext{
			identity:      identity,
			accountID:     cred.AccountID,
			principalType: principalTypeUser,
			userID:        live.UserID,
		}, nil
	}

	// Assumed-role, or a legacy record whose empty PrincipalType predates the
	// field — both mean "assumed-role" (mirrors resolveSessionAKID).
	if claims.PrincipalType != principalTypeAssumedRole {
		return principalContext{}, ecrInvalidPrincipal("principalType claim %q does not match resolved session", claims.PrincipalType)
	}

	canonicalARN, err := buildCallerARN(cred.AccountID, cred.SessionName, principalTypeAssumedRole, cred.AssumedRoleARN)
	if err != nil {
		return principalContext{}, ecrInvalidPrincipal("cannot build canonical ARN: %w", err)
	}
	if canonicalARN != claims.Subject {
		return principalContext{}, ecrInvalidPrincipal("subject claim does not match canonical principal ARN")
	}

	return principalContext{
		identity:          cred.SessionName,
		accountID:         cred.AccountID,
		principalType:     principalTypeAssumedRole,
		assumedRoleARN:    cred.AssumedRoleARN,
		assumedRoleID:     cred.AssumedRoleID,
		underlyingRoleARN: cred.UnderlyingRoleARN,
		userID:            cred.AssumedRoleID,
	}, nil
}

// requireActiveECRAccount fails closed unless accountID resolves to an
// active account, so a suspended account's already-issued tokens stop
// working immediately rather than at their natural 12h expiry.
func (gw *GatewayConfig) requireActiveECRAccount(accountID string) error {
	account, err := gw.IAMService.GetAccount(accountID)
	if err != nil {
		return classifyIAMLookupErr(err, "account")
	}
	if account.Status != handlers_iam.AccountStatusActive {
		return ecrInvalidPrincipal("account %q is not active", accountID)
	}
	return nil
}
