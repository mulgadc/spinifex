package gateway

import (
	"time"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
)

// policyRetryBase shortens the backoff between policy-resolve attempts.
var policyRetryBase = 200 * time.Millisecond

func init() {
	defaultCapabilities = func(iam iamdomain.IAMService) (PolicyEvaluator, AccountAdmission) {
		return policyauth.New(iam, policyauth.WithRetryBase(policyRetryBase)), accountadmission.New(iam)
	}
}
