package gateway_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// AssumeRole validates the request and delegates to STSService. Authorization
// takes two gates: the dispatcher checks the caller's identity policy against
// the target role ARN, and STSService checks the role's trust policy.
// callerARN is resolved by the dispatcher from the SigV4 principal.
func AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput, svc handlers_sts.STSService) (*sts.AssumeRoleOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.RoleArn) == "" || aws.StringValue(input.RoleSessionName) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AssumeRole(callerAccountID, callerARN, callerIdentity, input)
}
