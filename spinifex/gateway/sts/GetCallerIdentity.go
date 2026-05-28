package gateway_sts

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// Principal-type values mirror gateway.principalType{User,AssumedRole,Root}.
// Re-declared here to keep the gateway/sts sub-package free of an import cycle
// back to the parent gateway package; the dispatcher passes the string through.
const (
	PrincipalTypeUser        = "user"
	PrincipalTypeAssumedRole = "assumed-role"
	PrincipalTypeRoot        = "root"
)

// GetCallerIdentity resolves the caller's UserId and returns the AWS-shaped
// {Account, Arn, UserId} triple. Per AWS, every authenticated principal can
// call this — no policy gating. AWS UserId semantics:
//
//   - IAM user      → User.UserId  (AID... prefix)
//   - Assumed role  → AssumedRoleId ("{RoleID}:{SessionName}")
//   - Root          → the account ID
//
// callerAccessKeyID is the AKID from the SigV4 context (used to re-lookup the
// session credential for the AssumedRoleId on the ASIA path).
func GetCallerIdentity(
	accountID, callerARN, principalType, identity, callerAccessKeyID string,
	input *sts.GetCallerIdentityInput,
	iamSvc handlers_iam.IAMService,
	stsSvc handlers_sts.STSService,
) (*sts.GetCallerIdentityOutput, error) {
	userID, err := resolveCallerUserID(accountID, principalType, identity, callerAccessKeyID, iamSvc, stsSvc)
	if err != nil {
		return nil, err
	}
	return stsSvc.GetCallerIdentity(accountID, callerARN, userID, input)
}

func resolveCallerUserID(
	accountID, principalType, identity, callerAccessKeyID string,
	iamSvc handlers_iam.IAMService,
	stsSvc handlers_sts.STSService,
) (string, error) {
	switch principalType {
	case PrincipalTypeRoot:
		return accountID, nil
	case PrincipalTypeUser:
		// Root is currently encoded as principalType=user + identity="root"
		// (no separate root path through auth). Match AWS: root's UserId is
		// the account number.
		if identity == "root" {
			return accountID, nil
		}
		if iamSvc == nil {
			slog.Error("GetCallerIdentity: IAM service not initialized")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		out, err := iamSvc.GetUser(accountID, &iam.GetUserInput{UserName: aws.String(identity)})
		if err != nil {
			return "", err
		}
		if out == nil || out.User == nil || aws.StringValue(out.User.UserId) == "" {
			slog.Error("GetCallerIdentity: IAM GetUser returned empty UserId", "user", identity)
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return aws.StringValue(out.User.UserId), nil
	case PrincipalTypeAssumedRole:
		if stsSvc == nil {
			slog.Error("GetCallerIdentity: STS service not initialized")
			return "", errors.New(awserrors.ErrorInternalError)
		}
		cred, err := stsSvc.LookupSessionCredential(callerAccessKeyID)
		if err != nil {
			return "", err
		}
		if cred == nil {
			// SigV4 auth resolved this AKID a few milliseconds ago, so a miss
			// here means the janitor swept the record between auth and now —
			// surface as InvalidClientTokenId rather than leak a 500.
			slog.Warn("GetCallerIdentity: assumed-role session vanished between auth and dispatch", "akid", callerAccessKeyID)
			return "", errors.New(awserrors.ErrorInvalidClientTokenId)
		}
		return cred.AssumedRoleID, nil
	default:
		slog.Error("GetCallerIdentity: unknown principal type", "principalType", principalType)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}
