package awsgw

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const lambdaServicePrincipal = "lambda.amazonaws.com"

// lambdaRoleTrusts evaluates an execution role's stored trust policy for
// lambda.amazonaws.com acting for the function's account.
func lambdaRoleTrusts(iamSvc *iamdomain.IAMServiceImpl) func(accountID, roleARN string) (bool, error) {
	return func(accountID, roleARN string) (bool, error) {
		_, role, err := stsdomain.ResolveRoleByARN(iamSvc, roleARN)
		if errors.Is(err, stsdomain.ErrRoleUnresolved) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		err = stsdomain.ServiceTrusts(aws.StringValue(role.AssumeRolePolicyDocument), lambdaServicePrincipal, accountID)
		if err == nil {
			return true, nil
		}
		if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorAccessDenied {
			return false, nil
		}
		return false, err
	}
}
