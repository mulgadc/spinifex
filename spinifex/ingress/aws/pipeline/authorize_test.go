package pipeline_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePolicy records what Evaluate was asked and answers with verdict.
type fakePolicy struct {
	requestKeys iampolicy.ConditionKeys
	verdict     error

	gotAction    string
	gotResources []string
	gotKeys      iampolicy.ConditionKeys
}

func (f *fakePolicy) RequestConditionKeys(*http.Request, credential.Principal) iampolicy.ConditionKeys {
	return f.requestKeys
}

func (f *fakePolicy) Evaluate(_ credential.Principal, action string, resources []string, keys iampolicy.ConditionKeys) error {
	f.gotAction, f.gotResources, f.gotKeys = action, resources, keys
	return f.verdict
}

var alice = credential.Principal{AccountID: "123456789012", Identity: "alice"}

func authorize(p pipeline.PolicyEvaluator, caller credential.Principal, authenticated bool) error {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return pipeline.Authorize(p, r, caller, authenticated, "acm", "DeleteCertificate", []string{"arn:one"}, iampolicy.ConditionKeys{"k": "action"})
}

func TestAuthorize_FailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		p             pipeline.PolicyEvaluator
		caller        credential.Principal
		authenticated bool
		want          string
	}{
		"no evaluator":     {nil, alice, true, awserrors.ErrorInternalError},
		"unauthenticated":  {&fakePolicy{}, alice, false, awserrors.ErrorAccessDenied},
		"no identity name": {&fakePolicy{}, credential.Principal{AccountID: "1"}, true, awserrors.ErrorAccessDenied},
		"no account":       {&fakePolicy{}, credential.Principal{Identity: "alice"}, true, awserrors.ErrorInternalError},
	} {
		t.Run(name, func(t *testing.T) {
			err := authorize(tc.p, tc.caller, tc.authenticated)
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

func TestAuthorize_LayersActionKeysOverRequestKeysAndNamesTheIAMAction(t *testing.T) {
	p := &fakePolicy{requestKeys: iampolicy.ConditionKeys{"k": "request", "only": "request"}}

	require.NoError(t, authorize(p, alice, true))

	assert.Equal(t, "acm:DeleteCertificate", p.gotAction)
	assert.Equal(t, []string{"arn:one"}, p.gotResources)
	assert.Equal(t, iampolicy.ConditionKeys{"k": "action", "only": "request"}, p.gotKeys)
}

func TestAuthorize_ReturnsTheVerdict(t *testing.T) {
	denied := errors.New(awserrors.ErrorAccessDenied)
	assert.Equal(t, denied, authorize(&fakePolicy{verdict: denied, requestKeys: iampolicy.ConditionKeys{}}, alice, true))
}
