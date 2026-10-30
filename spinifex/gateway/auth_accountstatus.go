package gateway

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
)

// checkAccountActive rejects a request whose account is suspended or being
// torn down. It returns an empty string when the request may proceed.
//
// This is the only enforcement point for account status on the AWS surface:
// until it existed, suspending an account blocked ECR and nothing else.
func (gw *GatewayConfig) checkAccountActive(accountID string, src authSource) string {
	switch err := gw.Admission.Admit(accountID, src.logIP); {
	case err == nil:
		return ""
	case errors.Is(err, accountadmission.ErrNotActive):
		return awserrors.ErrorAccessDenied
	default:
		return awserrors.ErrorInternalError
	}
}
