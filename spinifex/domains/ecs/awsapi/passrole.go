package awsapi

import (
	"context"
	"errors"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
)

// ecsUnableToAssume is the ClientException text ECS gives when a RunTask role
// override names a role that does not exist.
const ecsUnableToAssume = "ECS was unable to assume the role '%s' that was provided for this task. " +
	"Please verify that the role being passed has the proper trust relationship and permissions " +
	"and that your IAM user has permissions to pass this role."

// accountRoles classifies a role reference by its account alone. ECS accepts a
// role that does not exist when a task definition is registered, so only a
// malformed or foreign reference is told apart.
type accountRoles struct{}

func (accountRoles) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	roleAccount, name, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return passrole.Role{}, passrole.ErrMalformed
	}
	if roleAccount != accountID {
		return passrole.Role{}, passrole.ErrForeignAccount
	}
	return passrole.Role{ARN: roleARN, Name: name}, nil
}

var _ passrole.Resolver = accountRoles{}

// roleChecker returns the PassRole check for roles named by a caller. A foreign
// role is refused before authorization, a role that is not an ARN is authorized
// as written and then accepted, and a missing role is reported with notFound
// only to a caller allowed to pass it.
func roleChecker(r passrole.Resolver, authorize passrole.AuthorizeFunc, accountID string, notFound func(roleARN string) error) PassRoleChecker {
	return func(roleARN string) error {
		_, err := passrole.Check(r, authorize, accountID, roleARN, tasksServicePrincipal)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, passrole.ErrForeignAccount):
			return awserrors.Errorf(awserrors.ErrorECSClientException, "Role is not valid")
		case errors.Is(err, passrole.ErrMalformed):
			return authorizePassRole(authorize, roleARN)
		case errors.Is(err, passrole.ErrNotFound) && notFound != nil:
			return notFound(roleARN)
		case errors.Is(err, passrole.ErrNotFound):
			return nil
		default:
			return err
		}
	}
}

func authorizePassRole(authorize passrole.AuthorizeFunc, roleARN string) error {
	_, err := passrole.Check(literalRole{}, authorize, "", roleARN, tasksServicePrincipal)
	return err
}

// literalRole resolves any reference to itself, so Check authorizes it as written.
type literalRole struct{}

func (literalRole) ResolveRole(_, roleARN string) (passrole.Role, error) {
	return passrole.Role{ARN: roleARN}, nil
}

var _ passrole.Resolver = literalRole{}

func unableToAssume(roleARN string) error {
	return awserrors.Errorf(awserrors.ErrorECSClientException, ecsUnableToAssume, roleARN)
}

type overrideRoleCheckKey struct{}

// withOverrideRoleCheck carries the check for roles a RunTask override names,
// which also reports a role that does not exist.
func withOverrideRoleCheck(ctx context.Context, check PassRoleChecker) context.Context {
	return context.WithValue(ctx, overrideRoleCheckKey{}, check)
}

func overrideRoleCheck(ctx context.Context) PassRoleChecker {
	check, _ := ctx.Value(overrideRoleCheckKey{}).(PassRoleChecker)
	return check
}
