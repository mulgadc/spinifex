package gateway_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
)

// GetSessionToken exchanges the caller's long-lived user credentials for
// short-lived session credentials for the SAME identity. Like GetCallerIdentity
// it is NOT gated by checkPolicy at the dispatcher (see stsSkipPolicyCheck) so
// SDK/console init flows are not blocked; the authoritative authorization check
// lives in the handler, which accepts only a long-lived user principal and
// denies assumed-role, root, and session callers per AWS semantics.
//
// The dispatcher supplies the caller fields from the SigV4 context —
// callerUserName is c.identity, callerPrincipalType is c.principalType. The
// handler validates the all-optional input (duration clamp, MFA rejection), so
// this wrapper is a thin, uniform passthrough mirroring the other STS actions.
func GetSessionToken(callerAccountID, callerUserName, callerPrincipalType string, input *sts.GetSessionTokenInput, svc handlers_sts.STSService) (*sts.GetSessionTokenOutput, error) {
	return svc.GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, input)
}
