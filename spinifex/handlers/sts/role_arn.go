package handlers_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"

	"github.com/mulgadc/bluebottle/pkg/auth"
	spxarn "github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// ErrRoleUnresolved reports a caller-supplied role ARN that names no role the
// caller may learn about: either no such role, or an ARN that is not the form
// IAM stored. Both are masked to AccessDenied on the wire.
var ErrRoleUnresolved = errors.New("role ARN does not resolve to a stored role")

// RoleGetter is the IAM surface role resolution needs.
type RoleGetter interface {
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

// CanonicalRoleARN returns the pathless ARN a supplied role ARN reduces to.
// Roles are keyed by account and name, so a path the caller wrote is not part
// of the role's identity and cannot be carried into an authorization decision.
func CanonicalRoleARN(roleARN string) (string, error) {
	accountID, roleName, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return "", errors.New(awserrors.ErrorValidationError)
	}
	return spxarn.FormatIAMPath(spxarn.IAMRole, accountID, "/", roleName), nil
}

// ResolveRoleByARN resolves the role a caller-supplied ARN names and verifies
// the ARN is the one IAM stored, returning the role's account. The lookup
// discards any path in the supplied ARN, so comparing the stored ARN back is
// what stops an invented path reaching a role the ARN does not name.
func ResolveRoleByARN(svc RoleGetter, roleARN string) (string, *iam.Role, error) {
	accountID, roleName, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return "", nil, errors.New(awserrors.ErrorValidationError)
	}
	out, err := svc.GetRole(accountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		if err.Error() == awserrors.ErrorIAMNoSuchEntity {
			return "", nil, ErrRoleUnresolved
		}
		return "", nil, err
	}
	if out == nil || out.Role == nil || aws.StringValue(out.Role.Arn) != roleARN {
		return "", nil, ErrRoleUnresolved
	}
	return accountID, out.Role, nil
}
