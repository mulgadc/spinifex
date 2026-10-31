package gateway

import (
	"net/http"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

// PolicyEvaluator is the IAM decision the authorization gate asks for: the
// condition keys a request carries, and the verdict for a principal, action,
// resources and keys.
type PolicyEvaluator interface {
	RequestConditionKeys(r *http.Request, principal credential.Principal) iampolicy.ConditionKeys
	Evaluate(principal credential.Principal, iamAction string, resources []string, keys iampolicy.ConditionKeys) error
}

// AccountAdmission decides whether the authenticated account may make requests.
// It returns nil to proceed and credential.ErrAccountNotActive to refuse; any
// other error is an outage.
type AccountAdmission interface {
	Admit(accountID, sourceIP string) error
}

// defaultCapabilities builds the capabilities a GatewayConfig left unset from
// its IAM service. Production composition sets both and leaves this nil.
var defaultCapabilities func(iamdomain.IAMService) (PolicyEvaluator, AccountAdmission)

// fillDefaultCapabilities sets whichever capability is nil from
// defaultCapabilities. With no defaults installed it changes nothing.
func (gw *GatewayConfig) fillDefaultCapabilities() {
	if defaultCapabilities == nil || (gw.Policy != nil && gw.Admission != nil) {
		return
	}
	policy, admission := defaultCapabilities(gw.IAMService)
	if gw.Policy == nil {
		gw.Policy = policy
	}
	if gw.Admission == nil {
		gw.Admission = admission
	}
}
