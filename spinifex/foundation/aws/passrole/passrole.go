// Package passrole is the shared iam:PassRole flow a service adapter runs
// before it hands a role to a managed service. It classifies the role
// reference and orders the checks so a caller who may not pass a role cannot
// learn whether it exists; it knows no service and writes no wire error.
package passrole

import (
	"errors"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
)

// Classifications a Resolver reports. Any other error is a dependency
// outage and is never mapped to a caller-facing refusal.
var (
	ErrMalformed      = errors.New("role reference is malformed")
	ErrForeignAccount = errors.New("role is not in the caller's account")
	ErrNotFound       = errors.New("role does not exist")
)

// Role is the canonical identity of a same-account role. It carries no
// record, trust policy or attached policy.
type Role struct {
	ARN  string
	Name string
}

// Resolver turns a caller-supplied role ARN into a canonical same-account role.
// It returns ErrMalformed, ErrForeignAccount or ErrNotFound (a spelling that
// is not the stored ARN is ErrNotFound) and any other error for an outage.
type Resolver interface {
	ResolveRole(accountID, roleARN string) (Role, error)
}

// AuthorizeFunc asks for an IAM decision on the authenticated caller. A nil
// error permits the operation.
type AuthorizeFunc func(service, action string, resources []string, keys iampolicy.ConditionKeys) error

// Check resolves roleARN for accountID and authorizes iam:PassRole on it with
// iam:PassedToService set to passedToService, which only the calling adapter
// may supply.
// A malformed or foreign reference is refused before any authorization.
// A missing role is authorized as written first, so only a caller allowed to
// pass it is told the role is absent.
// An outage is returned unchanged and nothing is authorized.
func Check(r Resolver, authorize AuthorizeFunc, accountID, roleARN, passedToService string) (Role, error) {
	role, err := r.ResolveRole(accountID, roleARN)
	switch {
	case err == nil:
		if err := passRole(authorize, role.ARN, passedToService); err != nil {
			return Role{}, err
		}
		return role, nil
	case errors.Is(err, ErrNotFound):
		if authErr := passRole(authorize, roleARN, passedToService); authErr != nil {
			return Role{}, authErr
		}
		return Role{}, err
	default:
		return Role{}, err
	}
}

func passRole(authorize AuthorizeFunc, roleARN, passedToService string) error {
	return authorize("iam", "PassRole", []string{roleARN},
		iampolicy.ConditionKeys{iampolicy.KeyPassedToService: passedToService})
}
