package gateway_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// GetSessionToken delegates to the STSService; not gated by caller IAM policy,
// as AWS requires no permission for authentication operations. callerUserName
// and callerAccessKeyID come from the SigV4 context.
func GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput, svc handlers_sts.STSService) (*sts.GetSessionTokenOutput, error) {
	return svc.GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID, input)
}
