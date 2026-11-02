package iam

import (
	"context"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
)

// RoleResolver resolves a caller-supplied role ARN to a canonical same-account
// role. It exposes only the ARN and name, never the stored record.
type RoleResolver struct {
	svc *IAMServiceImpl
}

var _ passrole.Resolver = (*RoleResolver)(nil)

// NewRoleResolver returns a resolver over the service's role store.
func NewRoleResolver(svc *IAMServiceImpl) *RoleResolver {
	return &RoleResolver{svc: svc}
}

// ResolveRole classifies roleARN against accountID. A spelling that differs
// from the stored ARN (a different path, say) is ErrNotFound.
func (r *RoleResolver) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	roleAccount, name, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return passrole.Role{}, passrole.ErrMalformed
	}
	if roleAccount != accountID {
		return passrole.Role{}, passrole.ErrForeignAccount
	}
	role, err := r.svc.getRole(context.Background(), accountID, name)
	if err != nil {
		if err.Error() == awserrors.ErrorIAMNoSuchEntity {
			return passrole.Role{}, passrole.ErrNotFound
		}
		return passrole.Role{}, err
	}
	if role.ARN != roleARN {
		return passrole.Role{}, passrole.ErrNotFound
	}
	return passrole.Role{ARN: role.ARN, Name: name}, nil
}
