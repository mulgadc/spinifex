//test:in-package — drives the unexported action table through Dispatch.

package awsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type allowQuota struct{}

var _ Quota = allowQuota{}

func (allowQuota) EnforceLaunch(context.Context, string, string, int) error { return nil }

func (allowQuota) ChargeLaunch(context.Context, string, *ec2.Reservation) error { return nil }

func (allowQuota) EnforceRetype(context.Context, *nats.Conn, func() []string, string, string, string) (int, error) {
	return 0, nil
}

func (allowQuota) AddVCPU(context.Context, string, int) error { return nil }

func (allowQuota) EnforceVolumeCreate(context.Context, *nats.Conn, string, int) error { return nil }

func (allowQuota) EnforceVolumeModify(context.Context, *nats.Conn, string, string, int) error {
	return nil
}

func (allowQuota) EnforceEIPs(context.Context, *nats.Conn, string, int) error { return nil }

func (allowQuota) EnforceSubnets(context.Context, *nats.Conn, string, int) error { return nil }

func (allowQuota) EnforceVPCs(context.Context, *nats.Conn, string, int) error { return nil }

type noProfiles struct{}

func (noProfiles) ResolveInstanceProfile(string, string) (*iamdomain.InstanceProfile, error) {
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (noProfiles) CanonicalResourceARN(string, arn.IAMResourceType, string) (string, error) {
	return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func allowAll(string, string, []string, iampolicy.ConditionKeys) error { return nil }

func formRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	// A daemon that answers every EC2 subject with an empty result, so each
	// action reaches its domain call and returns instead of waiting.
	sub, err := nc.Subscribe("ec2.>", func(m *nats.Msg) { _ = m.Respond([]byte("{}")) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	return NewHandler(Deps{
		NATS:          nc,
		IAM:           noProfiles{},
		Quota:         allowQuota{},
		ActiveNodes:   func(context.Context) int { return 1 },
		ExpectedNodes: 1,
		NodeIDs:       []string{"node-1"},
		AZ:            "ap-southeast-2a",
		Endpoint:      "https://localhost:9999",
	})
}

// Every routed action must reach its handler through Dispatch. Inputs are empty,
// so the domain call may reject them, but the wrapper must run and Dispatch must
// answer with a result or an error rather than panic.
func TestDispatch_EveryActionReachesItsHandler(t *testing.T) {
	h := newTestHandler(t)
	for _, action := range ActionNames() {
		t.Run(action, func(t *testing.T) {
			w := httptest.NewRecorder()
			inv := dispatch.Invocation{
				Request:   formRequest("Action=" + action),
				AccountID: testAccountID,
				Service:   ServiceName,
				Region:    testRegion,
				Authorize: allowAll,
			}
			assert.NotPanics(t, func() { _ = h.Dispatch(w, inv) })
		})
	}
}

func TestDispatch_RequestErrors(t *testing.T) {
	h := newTestHandler(t)
	deny := func(string, string, []string, iampolicy.ConditionKeys) error {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	cases := []struct {
		name      string
		body      string
		accountID string
		authorize dispatch.AuthorizeFunc
		want      string
	}{
		{"missing action", "Foo=bar", testAccountID, allowAll, awserrors.ErrorMissingAction},
		{"unknown action", "Action=NoSuchAction", testAccountID, allowAll, awserrors.ErrorInvalidAction},
		{"no account", "Action=DescribeVpcs", "", allowAll, awserrors.ErrorInternalError},
		{"policy denial", "Action=DescribeVpcs", testAccountID, deny, awserrors.ErrorUnauthorizedOperation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := h.Dispatch(httptest.NewRecorder(), dispatch.Invocation{
				Request: formRequest(tc.body), AccountID: tc.accountID, Service: ServiceName,
				Region: testRegion, Authorize: tc.authorize,
			})
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

func TestDispatch_SuccessWritesXML(t *testing.T) {
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	err := h.Dispatch(w, dispatch.Invocation{
		Request: formRequest("Action=DescribeRegions"), AccountID: testAccountID,
		Service: ServiceName, Region: testRegion, Authorize: allowAll,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/xml", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "ap-southeast-2")
}

// Without a NATS connection the gateway-local actions still answer and every
// other action reports ServerInternal.
func TestDispatch_NoNATS(t *testing.T) {
	h := NewHandler(Deps{AZ: "ap-southeast-2a", Endpoint: "https://localhost:9999"})
	inv := func(body string) dispatch.Invocation {
		return dispatch.Invocation{
			Request: formRequest(body), AccountID: testAccountID, Service: ServiceName,
			Region: testRegion, Authorize: allowAll,
		}
	}
	require.NoError(t, h.Dispatch(httptest.NewRecorder(), inv("Action=DescribeRegions")))
	err := h.Dispatch(httptest.NewRecorder(), inv("Action=DescribeVpcs"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestInputHasField(t *testing.T) {
	assert.True(t, InputHasField("CreateVpc", "DryRun"))
	assert.False(t, InputHasField("CreateVpc", "NoSuchField"))
	assert.False(t, InputHasField("NoSuchAction", "DryRun"))
}
