package credverify

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	spxarn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

var (
	_ credential.Rehydrator     = (*Verifier)(nil)
	_ credential.RoleRehydrator = (*Verifier)(nil)
)

// RehydrateRole rebuilds an assumed-role caller from the live role, without the
// session record: the role named by assumedRoleARN must still exist in accountID with
// the immutable roleID the caller was minted for. A deleted role, or one recreated
// under the same name with a new ID, is RoleGone. Nothing here reads the session store.
func (v *Verifier) RehydrateRole(accountID, roleID, assumedRoleARN string) (credential.Principal, *credential.Failure) {
	if v == nil || v.iam == nil {
		slog.Error("credential rehydrate role: IAM service not initialized")
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}
	roleName, sessionName, ok := parseAssumedRoleARN(assumedRoleARN, accountID)
	if !ok || roleID == "" {
		slog.Warn("credential rehydrate role: malformed request", "accountID", accountID, "assumedRoleARN", assumedRoleARN)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}

	out, err := v.iam.GetRole(accountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	switch {
	case err != nil && awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity),
		err == nil && (out == nil || out.Role == nil),
		err == nil && aws.StringValue(out.Role.RoleId) != roleID:
		slog.Warn("credential rehydrate role: role gone", "accountID", accountID, "roleID", roleID, "assumedRoleARN", assumedRoleARN)
		return credential.Principal{}, &credential.Failure{Code: awserrors.ErrorInvalidClientTokenId, RoleGone: true}
	case err != nil:
		slog.Error("credential rehydrate role: role lookup failed", "accountID", accountID, "roleID", roleID, "err", err)
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}

	slog.Info("credential rehydrate role: session gone, authorising from the live role",
		"accountID", accountID, "roleID", roleID, "assumedRoleARN", assumedRoleARN)
	return v.principalOf("", session{
		identity:          sessionName,
		accountID:         accountID,
		principalType:     stsdomain.PrincipalTypeAssumedRole,
		assumedRoleARN:    assumedRoleARN,
		assumedRoleID:     roleID + ":" + sessionName,
		underlyingRoleARN: aws.StringValue(out.Role.Arn),
	}, nil)
}

// parseAssumedRoleARN splits arn:aws:sts::<account>:assumed-role/<role>/<session>,
// requiring the account to be wantAccount.
func parseAssumedRoleARN(value, wantAccount string) (roleName, sessionName string, ok bool) {
	service, _, account, resource, ok := spxarn.Split(value)
	if !ok || service != "sts" || account != wantAccount {
		return "", "", false
	}
	parts := strings.Split(resource, "/")
	if len(parts) != 3 || parts[0] != "assumed-role" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Rehydrate rebuilds the caller accessKeyID names from the current IAM and STS
// records. It runs the lookups Authenticate and Resolve run, through the same
// helpers, but needs no secret, signature or security token. Failure fingerprints
// are set as for SigV4 and ignored by the caller.
func (v *Verifier) Rehydrate(accessKeyID, accountID string) (credential.Principal, *credential.Failure) {
	if v == nil || v.iam == nil {
		slog.Error("credential rehydrate: IAM service not initialized")
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}
	req := credential.Request{AccessKeyID: accessKeyID}

	var sess session
	switch {
	case strings.HasPrefix(accessKeyID, longLivedAKIDPrefix):
		ak, f := v.lookupAccessKey(req)
		if f != nil {
			return credential.Principal{}, f
		}
		if ak.AccountID != accountID {
			slog.Warn("credential rehydrate: access key account mismatch", "accessKeyID", accessKeyID)
			return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
		}
		sess = session{identity: ak.UserName, accountID: ak.AccountID, principalType: stsdomain.PrincipalTypeUser}
	case strings.HasPrefix(accessKeyID, sessionAKIDPrefix):
		cred, f := v.lookupSession(req)
		if f != nil {
			return credential.Principal{}, f
		}
		if cred.AccountID != accountID {
			slog.Warn("credential rehydrate: session account mismatch", "accessKeyID", accessKeyID)
			return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
		}
		sess = sessionOf(cred)
	default:
		slog.Warn("credential rehydrate: unknown AKID prefix", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}

	live, f := v.checkSessionPrincipal(req, sess)
	if f != nil {
		return credential.Principal{}, f
	}
	return v.principalOf(accessKeyID, sess, live)
}

// principalOf builds the caller sess describes: its fields, the user ID and the
// canonical ARN. Rehydrate and RehydrateRole both end here, so a rebuilt role session
// cannot differ from the one its own session record yields.
func (v *Verifier) principalOf(accessKeyID string, sess session, live *stsdomain.SessionPrincipal) (credential.Principal, *credential.Failure) {
	p := credential.Principal{
		Identity:          sess.identity,
		AccountID:         sess.accountID,
		PrincipalType:     sess.principalType,
		AssumedRoleARN:    sess.assumedRoleARN,
		AssumedRoleID:     sess.assumedRoleID,
		UnderlyingRoleARN: sess.underlyingRoleARN,
	}
	if f := v.rehydrateUser(&p, sess, live); f != nil {
		return credential.Principal{}, f
	}

	arn, err := callerARN(p)
	if err != nil {
		slog.Warn("credential rehydrate: cannot build caller ARN", "accessKeyID", accessKeyID, "err", err)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}
	p.CallerARN = arn
	return p, nil
}

// rehydrateUser fills the user fields. Unlike Resolve, a long-lived key whose user
// is gone is invalid, because a token must not outlive the user it was minted for,
// and the root user is read like any other rather than short-circuited.
func (v *Verifier) rehydrateUser(p *credential.Principal, sess session, live *stsdomain.SessionPrincipal) *credential.Failure {
	switch {
	case sess.principalType == stsdomain.PrincipalTypeAssumedRole:
		p.UserID = sess.assumedRoleID
	case sess.cred == nil:
		out, err := v.iam.GetUser(sess.accountID, &iam.GetUserInput{UserName: aws.String(sess.identity)})
		if err != nil {
			if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
				slog.Warn("credential rehydrate: user not found", "accountID", sess.accountID, "user", sess.identity)
				return fail(awserrors.ErrorInvalidClientTokenId)
			}
			slog.Error("credential rehydrate: user lookup failed", "accountID", sess.accountID, "user", sess.identity, "err", err)
			return fail(awserrors.ErrorInternalError)
		}
		if out == nil || out.User == nil {
			slog.Warn("credential rehydrate: user record empty", "accountID", sess.accountID, "user", sess.identity)
			return fail(awserrors.ErrorInvalidClientTokenId)
		}
		p.Identity = aws.StringValue(out.User.UserName)
		p.UserID, p.UserARN = aws.StringValue(out.User.UserId), aws.StringValue(out.User.Arn)
	case sess.principalType == stsdomain.PrincipalTypeUser:
		if live != nil {
			p.UserID, p.UserARN = live.UserID, live.UserARN
		}
	}
	return nil
}

// callerARN is the canonical ARN of p: the sealed role-session ARN, or the stored
// user ARN with path, or the pathless user ARN when no record supplies one. The bootstrap root user maps to the account root ARN.
func callerARN(p credential.Principal) (string, error) {
	switch p.PrincipalType {
	case stsdomain.PrincipalTypeAssumedRole:
		if p.AssumedRoleARN == "" {
			return "", errors.New("assumed-role principal without ARN")
		}
		return p.AssumedRoleARN, nil
	case stsdomain.PrincipalTypeUser:
		if p.Identity == "root" && p.AccountID == awsidentifiers.GlobalAccountID {
			return spxarn.FormatIAMRoot(p.AccountID), nil
		}
		if p.Identity == "" {
			return "", errors.New("user principal without identity")
		}
		if p.UserARN != "" {
			return p.UserARN, nil
		}
		return spxarn.FormatIAMPath(spxarn.IAMUser, p.AccountID, "/", p.Identity), nil
	default:
		return "", errors.New("unknown principal type " + p.PrincipalType)
	}
}
