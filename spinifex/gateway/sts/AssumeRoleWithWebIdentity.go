package gateway_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// AssumeRoleWithWebIdentity validates the request and delegates to STSService.
// This action is anonymous — the caller is identified by the OIDC token, not
// SigV4. The role's trust policy is the authoritative authorization check.
func AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput, svc stsdomain.STSService) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.RoleArn) == "" || aws.StringValue(input.RoleSessionName) == "" || aws.StringValue(input.WebIdentityToken) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AssumeRoleWithWebIdentity(input)
}
