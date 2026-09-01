package handlers_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"

	"github.com/mulgadc/bluebottle/pkg/auth"
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

// ResolveRoleByARN resolves the role a caller-supplied ARN names and verifies
// the ARN is the one IAM stored, returning the role's account. The lookup
// discards any path in the supplied ARN, so comparing the stored ARN back is
// what stops an invented path reaching a role the ARN does not name.
func ResolveRoleByARN(svc RoleGetter, roleARN string) (string, *iam.Role, error) {
	var role *iam.Role
	accountID, _, err := auth.ResolveRoleARN(roleARN, func(accountID, roleName string) (string, error) {
		out, err := svc.GetRole(accountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
		if err != nil {
			return "", err
		}
		if out == nil || out.Role == nil {
			return "", nil
		}
		role = out.Role
		return aws.StringValue(out.Role.Arn), nil
	})
	switch {
	case errors.Is(err, auth.ErrInvalidRoleARN):
		return "", nil, errors.New(awserrors.ErrorValidationError)
	case errors.Is(err, auth.ErrRoleARNMismatch):
		return "", nil, ErrRoleUnresolved
	case err != nil:
		if err.Error() == awserrors.ErrorIAMNoSuchEntity {
			return "", nil, ErrRoleUnresolved
		}
		return "", nil, err
	}
	return accountID, role, nil
}
