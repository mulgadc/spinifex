package handlers_sts

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
)

// GetCallerIdentity returns the resolved caller identity. The gateway passes the
// three identity strings extracted from the SigV4 context.
func (s *STSServiceImpl) GetCallerIdentity(callerAccountID, callerARN, callerUserID string, _ *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{
		Account: aws.String(callerAccountID),
		Arn:     aws.String(callerARN),
		UserId:  aws.String(callerUserID),
	}, nil
}
