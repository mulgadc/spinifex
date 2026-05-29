package handlers_sts

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
)

// GetCallerIdentity returns the resolved caller identity. AWS allows every
// authenticated principal to call this — the gateway layer extracts identity
// from the SigV4 context and passes the three plain strings in. The input
// struct is empty in the AWS SDK and exists only for SDK uniformity.
func (s *STSServiceImpl) GetCallerIdentity(callerAccountID, callerARN, callerUserID string, _ *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{
		Account: aws.String(callerAccountID),
		Arn:     aws.String(callerARN),
		UserId:  aws.String(callerUserID),
	}, nil
}
