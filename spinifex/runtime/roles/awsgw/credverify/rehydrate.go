package credverify

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	spxarn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

var _ credential.Rehydrator = (*Verifier)(nil)

// Rehydrate rebuilds the caller accessKeyID names from the current IAM and STS
// records. It runs the same key, session and user lookups as Authenticate and
// Resolve but needs no secret, signature or security token.
func (v *Verifier) Rehydrate(accessKeyID, accountID string) (credential.Principal, *credential.Failure) {
	if v == nil || v.iam == nil {
		slog.Error("credential rehydrate: IAM service not initialized")
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}
	switch {
	case strings.HasPrefix(accessKeyID, longLivedAKIDPrefix):
		return v.rehydrateLongLived(accessKeyID, accountID)
	case strings.HasPrefix(accessKeyID, sessionAKIDPrefix):
		return v.rehydrateSession(accessKeyID, accountID)
	default:
		slog.Warn("credential rehydrate: unknown AKID prefix", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}
}

func (v *Verifier) rehydrateLongLived(accessKeyID, accountID string) (credential.Principal, *credential.Failure) {
	ak, err := v.iam.LookupAccessKey(accessKeyID)
	if err != nil {
		return credential.Principal{}, lookupFailure(err, "access key", accessKeyID)
	}
	if ak.Status != iamdomain.AccessKeyStatusActive {
		slog.Warn("credential rehydrate: access key inactive", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}
	if ak.AccountID != accountID {
		slog.Warn("credential rehydrate: access key account mismatch", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}

	out, err := v.iam.GetUser(ak.AccountID, &iam.GetUserInput{UserName: aws.String(ak.UserName)})
	if err != nil {
		return credential.Principal{}, lookupFailure(err, "user", accessKeyID)
	}
	if out == nil || out.User == nil {
		slog.Warn("credential rehydrate: user record empty", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}
	p := credential.Principal{
		Identity:      aws.StringValue(out.User.UserName),
		AccountID:     ak.AccountID,
		PrincipalType: stsdomain.PrincipalTypeUser,
		UserID:        aws.StringValue(out.User.UserId),
		UserARN:       aws.StringValue(out.User.Arn),
	}
	return withCallerARN(p, accessKeyID)
}

func (v *Verifier) rehydrateSession(accessKeyID, accountID string) (credential.Principal, *credential.Failure) {
	if v.sts == nil {
		slog.Error("credential rehydrate: STS service not initialized", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}
	cred, err := v.sts.LookupSessionCredential(accessKeyID)
	if err != nil {
		slog.Error("credential rehydrate: STS lookup failed", "accessKeyID", accessKeyID, "err", err)
		return credential.Principal{}, fail(awserrors.ErrorInternalError)
	}
	if cred == nil {
		slog.Warn("credential rehydrate: session not found", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}
	if time.Now().UTC().After(cred.ExpiresAt) {
		slog.Warn("credential rehydrate: session expired", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorExpiredToken)
	}
	if cred.AccountID != accountID {
		slog.Warn("credential rehydrate: session account mismatch", "accessKeyID", accessKeyID)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}

	live, err := v.sts.VerifySessionPrincipal(cred)
	if err != nil {
		if !stsdomain.IsSessionPrincipalVerdict(err) {
			slog.Error("credential rehydrate: session principal check faulted", "accessKeyID", accessKeyID, "err", err)
			return credential.Principal{}, fail(awserrors.ErrorInternalError)
		}
		slog.Warn("credential rehydrate: session principal no longer valid", "accessKeyID", accessKeyID, "reason", err)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}

	if cred.PrincipalType == stsdomain.PrincipalTypeUser {
		p := credential.Principal{
			Identity:      cred.SessionName,
			AccountID:     cred.AccountID,
			PrincipalType: stsdomain.PrincipalTypeUser,
		}
		if live != nil {
			p.UserID, p.UserARN = live.UserID, live.UserARN
		}
		return withCallerARN(p, accessKeyID)
	}

	// An empty PrincipalType predates the field and means assumed-role.
	return withCallerARN(credential.Principal{
		Identity:          cred.SessionName,
		AccountID:         cred.AccountID,
		PrincipalType:     stsdomain.PrincipalTypeAssumedRole,
		AssumedRoleARN:    cred.AssumedRoleARN,
		AssumedRoleID:     cred.AssumedRoleID,
		UnderlyingRoleARN: cred.UnderlyingRoleARN,
		UserID:            cred.AssumedRoleID,
	}, accessKeyID)
}

// lookupFailure maps an IAM lookup error: a missing record is an invalid
// credential, anything else says nothing about validity and is a dependency fault.
func lookupFailure(err error, what, accessKeyID string) *credential.Failure {
	if awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity) {
		slog.Warn("credential rehydrate: record not found", "what", what, "accessKeyID", accessKeyID)
		return fail(awserrors.ErrorInvalidClientTokenId)
	}
	slog.Error("credential rehydrate: lookup failed", "what", what, "accessKeyID", accessKeyID, "err", err)
	return fail(awserrors.ErrorInternalError)
}

func withCallerARN(p credential.Principal, accessKeyID string) (credential.Principal, *credential.Failure) {
	arn, err := callerARN(p)
	if err != nil {
		slog.Warn("credential rehydrate: cannot build caller ARN", "accessKeyID", accessKeyID, "err", err)
		return credential.Principal{}, fail(awserrors.ErrorInvalidClientTokenId)
	}
	p.CallerARN = arn
	return p, nil
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
