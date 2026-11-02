package sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ServiceTrusts reports whether trustDoc lets service assume the role on behalf
// of sourceAccount, with the same deny-first evaluation AssumeRole uses. A nil
// error means trusted; a refusal is AccessDenied.
func ServiceTrusts(trustDoc, service, sourceAccount string) error {
	return evalTrustPolicyFor(trustDoc, "", sourceAccount, []string{service})
}

// RoleTrusts evaluates a role's stored trust policy for service acting for an
// account: false is a refusal (or no such role), an error an outage.
func RoleTrusts(svc RoleGetter, service string) func(accountID, roleARN string) (bool, error) {
	return func(accountID, roleARN string) (bool, error) {
		_, role, err := ResolveRoleByARN(svc, roleARN)
		if errors.Is(err, ErrRoleUnresolved) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		err = ServiceTrusts(aws.StringValue(role.AssumeRolePolicyDocument), service, accountID)
		if err == nil {
			return true, nil
		}
		if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorAccessDenied {
			return false, nil
		}
		return false, err
	}
}
