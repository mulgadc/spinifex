// Package awsapi implements the STS actions: it validates each request
// and delegates it to the STS service, which owns trust-policy checks and
// credential issuance.
package awsapi

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/sts"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// GetCallerIdentity returns the AWS {Account, Arn, UserId} triple. No policy
// gating — every authenticated principal may call this. UserId semantics:
//
//   - IAM user      → User.UserId  (AID... prefix)
//   - Assumed role  → AssumedRoleId ("{RoleID}:{SessionName}")
//   - Root          → the account ID
//
// assumedRoleID is resolved by the SigV4 middleware on the ASIA path; empty
// for non-session principals.
func GetCallerIdentity(
	accountID, callerARN, callerPrincipalType, identity, assumedRoleID string,
	input *sts.GetCallerIdentityInput,
	iamSvc iamdomain.IAMService,
	stsSvc stsdomain.STSService,
) (*sts.GetCallerIdentityOutput, error) {
	userID, _, err := stsdomain.ResolveCallerUser(accountID, callerPrincipalType, identity, assumedRoleID, iamSvc)
	if err != nil {
		return nil, err
	}
	if userID == "" {
		if callerPrincipalType == stsdomain.PrincipalTypeAssumedRole {
			// Session vanished between auth and dispatch (expired record); surface as
			// InvalidClientTokenId rather than a 500.
			slog.Warn("GetCallerIdentity: assumed-role session vanished between auth and dispatch")
			return nil, errors.New(awserrors.ErrorInvalidClientTokenId)
		}
		slog.Error("GetCallerIdentity: IAM GetUser returned empty UserId", "user", identity)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	return stsSvc.GetCallerIdentity(accountID, callerARN, userID, input)
}
