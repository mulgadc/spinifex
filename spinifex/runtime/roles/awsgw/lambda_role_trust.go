package awsgw

import (
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
)

const lambdaServicePrincipal = "lambda.amazonaws.com"

// lambdaRoleTrusts evaluates an execution role's stored trust policy for
// lambda.amazonaws.com acting for the function's account.
func lambdaRoleTrusts(svc stsdomain.RoleGetter) func(accountID, roleARN string) (bool, error) {
	return stsdomain.RoleTrusts(svc, lambdaServicePrincipal)
}
