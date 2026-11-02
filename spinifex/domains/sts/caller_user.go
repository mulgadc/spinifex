package sts

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Principal-type values an authenticated caller carries.
const (
	PrincipalTypeUser        = "user"
	PrincipalTypeAssumedRole = "assumed-role"
	PrincipalTypeRoot        = "root"
)

// ResolveCallerUser resolves the UserId of an authenticated principal, which is
// also aws:userid at the authorization door, and for an IAM user its stored ARN.
// An error is a dependency fault; a missing value returns empty.
func ResolveCallerUser(
	accountID, callerPrincipalType, identity, assumedRoleID string,
	iamSvc iamdomain.IAMService,
) (userID, userARN string, err error) {
	switch callerPrincipalType {
	case PrincipalTypeRoot:
		return accountID, "", nil
	case PrincipalTypeUser:
		// Root is encoded as principalType=user + identity="root"; UserId is the account ID.
		if identity == "root" {
			return accountID, "", nil
		}
		if iamSvc == nil {
			slog.Error("ResolveCallerUser: IAM service not initialized")
			return "", "", errors.New(awserrors.ErrorInternalError)
		}
		out, err := iamSvc.GetUser(accountID, &iam.GetUserInput{UserName: aws.String(identity)})
		if err != nil {
			return "", "", err
		}
		if out == nil || out.User == nil {
			return "", "", nil
		}
		return aws.StringValue(out.User.UserId), aws.StringValue(out.User.Arn), nil
	case PrincipalTypeAssumedRole:
		return assumedRoleID, "", nil
	default:
		slog.Error("ResolveCallerUser: unknown principal type", "principalType", callerPrincipalType)
		return "", "", errors.New(awserrors.ErrorInternalError)
	}
}
