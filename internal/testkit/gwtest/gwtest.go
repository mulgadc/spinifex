// Package gwtest builds the invocation a registered AWS service receives from
// the ingress pipeline, so a service's handler tests run in the service's own
// package with real policy evaluation and no gateway.
package gwtest

import (
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
)

const (
	// AccountID is the account the default caller belongs to.
	AccountID = "123456789012"
	// Region is the region the default harness serves.
	Region = "ap-southeast-2"
)

// policyIAM resolves fixed policy documents for every user and role. Only the
// two methods the policy evaluator reads are implemented; any other call on
// the embedded nil interface panics, which is the point.
type policyIAM struct {
	iamdomain.IAMService

	resolve func(accountID, name string) ([]iamdomain.PolicyDocument, error)
}

func (p policyIAM) GetUserPolicies(accountID, name string) ([]iamdomain.PolicyDocument, error) {
	return p.resolve(accountID, name)
}

func (p policyIAM) GetRolePolicies(accountID, name string) ([]iamdomain.PolicyDocument, error) {
	return p.resolve(accountID, name)
}

// Statement is one policy statement naming a single action and resource.
func Statement(effect, action, resource string) iamdomain.Statement {
	return iamdomain.Statement{
		Effect:   effect,
		Action:   iamdomain.StringOrArr{action},
		Resource: iamdomain.StringOrArr{resource},
	}
}

// Harness invokes a registration as the pipeline would for one authenticated
// user, authorizing through the real policy evaluator.
type Harness struct {
	// AccountID is the caller's account as the invocation carries it.
	AccountID string
	Region    string
	Policy    pipeline.PolicyEvaluator
	// Caller overrides the default caller, an IAM user named alice.
	Caller *credential.Principal
}

// Scoped returns a harness whose caller holds one policy of statements.
func Scoped(statements ...iamdomain.Statement) *Harness {
	return FromDocs([]iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}})
}

// FromDocs returns a harness whose caller holds exactly these policy documents.
func FromDocs(docs []iamdomain.PolicyDocument) *Harness {
	return FromResolver(func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil })
}

// FromResolver returns a harness whose caller's policies come from resolve,
// which sees every user or role lookup, so a test can count them.
func FromResolver(resolve func(accountID, name string) ([]iamdomain.PolicyDocument, error)) *Harness {
	return &Harness{
		AccountID: AccountID,
		Region:    Region,
		Policy:    policyauth.New(policyIAM{resolve: resolve}),
	}
}

// AllowAll returns a harness whose caller may do anything.
func AllowAll() *Harness {
	return Scoped(Statement("Allow", "*", "*"))
}

// Dispatch hands r to reg as an invocation for service, authorizing through
// the pipeline's own gate over h's policy.
func (h *Harness) Dispatch(reg dispatch.Registration, service string, w http.ResponseWriter, r *http.Request) error {
	caller := credential.Principal{
		AccountID:     h.AccountID,
		Identity:      "alice",
		PrincipalType: stsdomain.PrincipalTypeUser,
		CallerARN:     "arn:aws:iam::" + h.AccountID + ":user/alice",
	}
	if h.Caller != nil {
		caller = *h.Caller
	}
	return reg.Dispatch(w, dispatch.Invocation{
		Request:   r,
		AccountID: h.AccountID,
		Service:   service,
		Region:    h.Region,
		Actor:     func() (dispatch.Actor, error) { return pipeline.ActorFor(caller, "") },
		Authorize: func(svc, action string, resources []string, actionKeys iampolicy.ConditionKeys) error {
			return pipeline.Authorize(h.Policy, r, caller, true, svc, action, resources, actionKeys)
		},
	})
}
