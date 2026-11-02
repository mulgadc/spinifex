package pipeline

//test:in-package — the gate steps under test are unexported and take their
// capabilities through the PolicyEvaluator and AccountAdmission interfaces.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePolicy struct {
	err       error
	gotAction string
	gotKeys   iampolicy.ConditionKeys
}

func (f *fakePolicy) RequestConditionKeys(*http.Request, credential.Principal) iampolicy.ConditionKeys {
	return iampolicy.ConditionKeys{iampolicy.KeyUsername: "request-key"}
}

func (f *fakePolicy) Evaluate(_ credential.Principal, action string, _ []string, keys iampolicy.ConditionKeys) error {
	f.gotAction, f.gotKeys = action, keys
	return f.err
}

type fakeAdmission struct{ err error }

func (f fakeAdmission) Admit(string, string) error { return f.err }

func TestCheckPolicy_UsesTheSuppliedEvaluator(t *testing.T) {
	policy := &fakePolicy{}
	gw := &Config{Policy: policy}
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/", nil))
	req = req.WithContext(withPrincipalField(req.Context(), fieldAccountID, "123456789012"))

	err := gw.checkPolicyResourcesWithKeys(req, "ec2", "DescribeVpcs", []string{"*"}, iampolicy.ConditionKeys{iampolicy.KeyUserID: "action-key"})

	require.NoError(t, err)
	assert.Equal(t, "ec2:DescribeVpcs", policy.gotAction)
	assert.Equal(t, "request-key", policy.gotKeys[iampolicy.KeyUsername], "request keys come from the evaluator")
	assert.Equal(t, "action-key", policy.gotKeys[iampolicy.KeyUserID], "action keys are layered over them")
}

func TestCheckPolicy_ReturnsTheEvaluatorVerdict(t *testing.T) {
	verdict := errors.New(awserrors.ErrorAccessDenied)
	gw := &Config{Policy: &fakePolicy{err: verdict}}
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/", nil))
	req = req.WithContext(withPrincipalField(req.Context(), fieldAccountID, "123456789012"))

	assert.ErrorIs(t, gw.checkPolicyResources(req, "ec2", "DescribeVpcs", []string{"*"}), verdict)
}

func TestCheckPolicy_NoEvaluatorFailsClosed(t *testing.T) {
	gw := &Config{}
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/", nil))

	err := gw.checkPolicyResources(req, "ec2", "DescribeVpcs", []string{"*"})

	assert.EqualError(t, err, awserrors.ErrorInternalError)
}

func TestCheckAccountActive_MapsAdmissionVerdicts(t *testing.T) {
	src := authSource{}
	cases := []struct {
		name string
		gw   *Config
		want string
	}{
		{"active proceeds", &Config{Admission: fakeAdmission{}}, ""},
		{"not active is denied", &Config{Admission: fakeAdmission{err: credential.ErrAccountNotActive}}, awserrors.ErrorAccessDenied},
		{"an outage is an internal error", &Config{Admission: fakeAdmission{err: errors.New("iam down")}}, awserrors.ErrorInternalError},
		{"no admission fails closed", &Config{}, awserrors.ErrorInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.gw.checkAccountActive("123456789012", src))
		})
	}
}
