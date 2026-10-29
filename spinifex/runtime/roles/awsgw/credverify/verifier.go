// Package credverify verifies SigV4 credentials against the IAM and STS stores for
// the gateway's authenticator. The secret is read and used here and never returned.
package credverify

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
)

// AKID prefixes. Prefix-first dispatch prevents a misfiled record from being
// resolved by the wrong lookup path.
const (
	longLivedAKIDPrefix = "AKIA"
	sessionAKIDPrefix   = "ASIA"
)

var _ credential.Verifier = (*Verifier)(nil)

// Verifier resolves credentials through the IAM and STS services.
type Verifier struct {
	iam iamdomain.IAMService
	sts stsdomain.STSService
}

// New returns a Verifier. sts may be nil on a node that issues no sessions; a
// session credential then fails as an internal error rather than authenticating.
func New(iam iamdomain.IAMService, sts stsdomain.STSService) *Verifier {
	return &Verifier{iam: iam, sts: sts}
}

// session is the Handle Authenticate returns for Resolve.
type session struct {
	identity          string
	accountID         string
	principalType     string
	assumedRoleARN    string
	assumedRoleID     string
	underlyingRoleARN string
	// cred is the rehydrated STS record on the ASIA path, nil on the long-lived one.
	cred *stsdomain.SessionCredential
}

func fail(code string) *credential.Failure { return &credential.Failure{Code: code} }

func failCounted(code, kind string, parts ...string) *credential.Failure {
	return &credential.Failure{Code: code, FingerprintKind: kind, FingerprintParts: parts}
}

// Authenticate resolves the secret for req.AccessKeyID, hands it to verifySig and,
// only if that accepts, returns the credential's account.
func (v *Verifier) Authenticate(req credential.Request, verifySig func(secret string) error) (credential.Authenticated, *credential.Failure) {
	if v.iam == nil {
		slog.Error("SigV4 auth: IAM service not initialized")
		return credential.Authenticated{}, fail(awserrors.ErrorInternalError)
	}

	var (
		secret string
		sess   session
		f      *credential.Failure
	)
	switch {
	case strings.HasPrefix(req.AccessKeyID, longLivedAKIDPrefix):
		secret, sess, f = v.resolveLongLived(req)
	case strings.HasPrefix(req.AccessKeyID, sessionAKIDPrefix):
		secret, sess, f = v.resolveSession(req)
	default:
		slog.Warn("Auth failure: unknown AKID prefix", "accessKeyID", req.AccessKeyID, "sourceIP", req.SourceIP)
		return credential.Authenticated{}, failCounted(awserrors.ErrorInvalidClientTokenId, "unknown-prefix", req.AccessKeyID)
	}
	if f != nil {
		return credential.Authenticated{}, f
	}

	if err := verifySig(secret); err != nil {
		return credential.Authenticated{}, &credential.Failure{
			Code: awserrors.ErrorSignatureDoesNotMatch, SignatureMismatch: true,
		}
	}
	return credential.Authenticated{AccountID: sess.accountID, Handle: sess}, nil
}

// Resolve re-verifies a session's backing principal and resolves the caller's user ID.
func (v *Verifier) Resolve(req credential.Request, auth credential.Authenticated) (credential.Principal, *credential.Failure) {
	sess, ok := auth.Handle.(session)
	if !ok {
		slog.Error("SigV4 auth: Resolve called with a handle this verifier did not issue")
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}
	if f := v.checkSessionPrincipal(req, sess); f != nil {
		return credential.Principal{}, f
	}

	p := credential.Principal{
		Identity:          sess.identity,
		AccountID:         sess.accountID,
		PrincipalType:     sess.principalType,
		AssumedRoleARN:    sess.assumedRoleARN,
		AssumedRoleID:     sess.assumedRoleID,
		UnderlyingRoleARN: sess.underlyingRoleARN,
	}
	userID, userARN, err := v.principalUser(sess)
	if err != nil {
		return credential.Principal{}, fail(err.Error())
	}
	p.UserID, p.UserARN = userID, userARN
	return p, nil
}

// resolveLongLived handles the AKIA path: IAM lookup, status check, secret decrypt.
func (v *Verifier) resolveLongLived(req credential.Request) (string, session, *credential.Failure) {
	accessKeyID := req.AccessKeyID
	ak, err := v.iam.LookupAccessKey(accessKeyID)
	if err != nil {
		if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
			slog.Warn("Auth failure: access key not found", "accessKeyID", accessKeyID, "sourceIP", req.SourceIP)
			return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "akid-not-found", accessKeyID)
		}
		slog.Error("IAM lookup failed", "accessKeyID", accessKeyID, "err", err)
		return "", session{}, fail(awserrors.ErrorInternalError)
	}
	if ak.Status != iamdomain.AccessKeyStatusActive {
		slog.Warn("Auth failure: access key inactive", "accessKeyID", accessKeyID, "sourceIP", req.SourceIP)
		return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "akid-inactive", accessKeyID)
	}
	secret, err := v.iam.DecryptSecret(ak.SecretAccessKey)
	if err != nil {
		// Undecryptable secret (e.g. master key rotated): treat as auth failure, not
		// server fault, so the client re-authenticates instead of retrying a dead request.
		slog.Error("Failed to decrypt IAM secret", "accessKeyID", accessKeyID, "err", err)
		return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "akid-secret", accessKeyID)
	}
	return secret, session{
		identity:      ak.UserName,
		accountID:     ak.AccountID,
		principalType: stsdomain.PrincipalTypeUser,
	}, nil
}

// resolveSession handles the ASIA path: STS lookup, expiry check,
// X-Amz-Security-Token HMAC verify, secret decrypt. A nil STS service surfaces
// as InternalError so misconfiguration is loud at startup.
func (v *Verifier) resolveSession(req credential.Request) (string, session, *credential.Failure) {
	accessKeyID := req.AccessKeyID
	if v.sts == nil {
		slog.Error("SigV4 auth: STS service not initialized but session AKID presented", "accessKeyID", accessKeyID)
		return "", session{}, fail(awserrors.ErrorInternalError)
	}
	cred, err := v.sts.LookupSessionCredential(accessKeyID)
	if err != nil {
		slog.Error("STS lookup failed", "accessKeyID", accessKeyID, "err", err)
		return "", session{}, fail(awserrors.ErrorInternalError)
	}
	if cred == nil {
		slog.Warn("Auth failure: session credential not found", "accessKeyID", accessKeyID, "sourceIP", req.SourceIP)
		return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "session-not-found", accessKeyID)
	}
	if time.Now().UTC().After(cred.ExpiresAt) {
		slog.Warn("Auth failure: session credential expired",
			"accessKeyID", accessKeyID, "sourceIP", req.SourceIP, "expiresAt", cred.ExpiresAt)
		return "", session{}, failCounted(awserrors.ErrorExpiredToken, "session-expired", accessKeyID)
	}

	if req.SecurityToken == "" {
		slog.Warn("Auth failure: session AKID presented without X-Amz-Security-Token",
			"accessKeyID", accessKeyID, "sourceIP", req.SourceIP)
		return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "session-no-token", accessKeyID)
	}
	if !v.sts.VerifySessionToken(cred, req.SecurityToken) {
		slog.Warn("Auth failure: session token HMAC mismatch",
			"accessKeyID", accessKeyID, "sourceIP", req.SourceIP)
		return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "session-token-mismatch",
			accessKeyID, authlimit.TokenDigest(req.SecurityToken))
	}

	secret, err := v.iam.DecryptSecret(cred.SecretEncrypted)
	if err != nil {
		// Unverifiable secret: same auth-failure reasoning as resolveLongLived.
		slog.Error("Failed to decrypt session secret", "accessKeyID", accessKeyID, "err", err)
		return "", session{}, failCounted(awserrors.ErrorInvalidClientTokenId, "session-secret", accessKeyID)
	}
	if cred.PrincipalType == stsdomain.PrincipalTypeUser {
		// GetSessionToken session: resolve to the user so policy is evaluated as a user.
		return secret, session{
			identity:      cred.SessionName,
			accountID:     cred.AccountID,
			principalType: stsdomain.PrincipalTypeUser,
			cred:          cred,
		}, nil
	}

	// "assumed-role" or empty (pre-PrincipalType records) — both resolve as role session.
	return secret, session{
		identity:          cred.SessionName,
		accountID:         cred.AccountID,
		principalType:     stsdomain.PrincipalTypeAssumedRole,
		assumedRoleARN:    cred.AssumedRoleARN,
		assumedRoleID:     cred.AssumedRoleID,
		underlyingRoleARN: cred.UnderlyingRoleARN,
		cred:              cred,
	}, nil
}

// checkSessionPrincipal re-verifies that a session's backing principal still
// exists with the immutable ID the session was minted for. A long-lived
// credential (nil cred) is a no-op.
//
// Deliberately not part of resolveSession: that runs before the signature
// because it must produce the secret, whereas this reads the IAM users and roles
// buckets. An AKID travels in cleartext, so running it there would let anyone
// holding a copied one drive IAM reads with garbage signatures.
func (v *Verifier) checkSessionPrincipal(req credential.Request, sess session) *credential.Failure {
	if sess.cred == nil {
		return nil
	}
	if v.sts == nil {
		slog.Error("SigV4 auth: STS service not initialized", "accessKeyID", req.AccessKeyID)
		return fail(awserrors.ErrorInternalError)
	}

	if _, err := v.sts.VerifySessionPrincipal(sess.cred); err != nil {
		if !stsdomain.IsSessionPrincipalVerdict(err) {
			slog.Error("Session principal continuity check faulted, refusing to authorize",
				"accessKeyID", req.AccessKeyID, "err", err)
			return fail(awserrors.ErrorInternalError)
		}
		// InvalidClientTokenId, not ExpiredToken: the credential has not expired,
		// the principal behind it is no longer the one it was minted for.
		slog.Warn("Auth failure: session principal continuity check failed",
			"accessKeyID", req.AccessKeyID, "sourceIP", req.SourceIP,
			"identity", sess.identity, "principalType", sess.principalType,
			"reason", err)
		// Every legacy record fails at once on the deploy that ships this check, so
		// counting them would lock out a shared egress address on distinct-attempt
		// volume alone and answer "retry later" to a fleet that cannot act on it.
		if errors.Is(err, stsdomain.ErrSessionPrincipalLegacy) {
			return fail(awserrors.ErrorInvalidClientTokenId)
		}
		return failCounted(awserrors.ErrorInvalidClientTokenId, "session-principal", req.AccessKeyID)
	}
	return nil
}

// principalUser resolves aws:userid and an IAM user's stored ARN once, where the
// principal is resolved, so the value cannot differ between two policy checks.
func (v *Verifier) principalUser(sess session) (userID, userARN string, err error) {
	if sess.identity == "" || sess.accountID == "" {
		return "", "", nil
	}
	userID, userARN, err = stsdomain.ResolveCallerUser(sess.accountID, sess.principalType,
		sess.identity, sess.assumedRoleID, v.iam)
	switch {
	case err == nil:
		if userID == "" {
			slog.Warn("aws:userid unavailable: principal record carries no user ID",
				"accountID", sess.accountID, "identity", sess.identity,
				"principalType", sess.principalType)
		}
		return userID, userARN, nil
	case strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity):
		slog.Warn("aws:userid unavailable: no such IAM user",
			"accountID", sess.accountID, "user", sess.identity)
		return "", "", nil
	default:
		slog.Error("aws:userid: IAM dependency fault, refusing to authorize on a degraded context",
			"accountID", sess.accountID, "identity", sess.identity, "err", err)
		return "", "", errors.New(awserrors.ErrorInternalError)
	}
}
