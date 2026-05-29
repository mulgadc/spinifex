package gateway_sts

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// AssumeRole validates the inbound STS AssumeRole request and delegates to the
// STSService. The trust policy on the target role is the authoritative
// authorization check — the gateway deliberately does NOT call
// checkPolicy("sts","AssumeRole") here. This is the only spinifex action
// where the resource's own policy fully gates access, matching AWS behaviour.
//
// callerARN is built by the dispatcher from the SigV4 principal so the handler
// stays free of request-context plumbing.
func AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput, svc handlers_sts.STSService) (*sts.AssumeRoleOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.RoleArn) == "" || aws.StringValue(input.RoleSessionName) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AssumeRole(callerAccountID, callerARN, callerIdentity, input)
}
