// Package pipeline holds the capabilities the AWS request pipeline consumes.
// Runtime composition supplies the implementations.
package pipeline

import (
	"net/http"

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

// Readiness reports whether the message bus the services depend on is
// reachable. Nil means no readiness is configured.
type Readiness interface {
	Available() bool
}
