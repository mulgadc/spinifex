package gateway_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// GetSessionToken delegates to the STSService. Like GetCallerIdentity it is not
// gated by checkPolicy at the dispatcher (see stsSkipPolicyCheck); the handler
// holds the authoritative user-only check. Caller fields come from the SigV4
// context: callerUserName is c.identity, callerAccessKeyID is c.accessKey.
func GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput, svc handlers_sts.STSService) (*sts.GetSessionTokenOutput, error) {
	return svc.GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID, input)
}
