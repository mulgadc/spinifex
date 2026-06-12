package gateway_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// AssumeRoleWithWebIdentity validates the request and delegates to STSService.
// This action is anonymous — the caller is identified by the OIDC token, not
// SigV4. The role's trust policy is the authoritative authorization check.
func AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput, svc handlers_sts.STSService) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.RoleArn) == "" || aws.StringValue(input.RoleSessionName) == "" || aws.StringValue(input.WebIdentityToken) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AssumeRoleWithWebIdentity(input)
}
