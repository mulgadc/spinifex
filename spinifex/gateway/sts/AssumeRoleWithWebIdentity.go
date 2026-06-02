package gateway_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// AssumeRoleWithWebIdentity validates the inbound STS
// AssumeRoleWithWebIdentity request and delegates to the STSService. Unlike
// AssumeRole, this action is anonymous — the caller is identified by the
// supplied OIDC token, not by SigV4 — so no caller ARN / account is required
// on entry. The trust policy on the target role is the authoritative
// authorization check, evaluated inside the handler.
//
// The action is registered on the standard STS gateway dispatch but its
// authorization model is different: callers do not need to be SigV4-signed.
// The current STS dispatcher still threads requests through resolveSTSCaller
// which expects an authenticated principal; switching the action to an
// anonymous route group lands in Sprint 6e alongside the awsgw OIDC mux
// rework (eks-v1.md Q5).
func AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput, svc handlers_sts.STSService) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.RoleArn) == "" || aws.StringValue(input.RoleSessionName) == "" || aws.StringValue(input.WebIdentityToken) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AssumeRoleWithWebIdentity(input)
}
