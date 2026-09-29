//test:in-package — walks the unexported ec2Actions route table and drives
// EC2_Request through the gateway's unexported test helpers.

package gateway

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dryRunMessage = "Request would have succeeded, but DryRun flag is set."

// modelsDryRun reports whether an action's SDK input carries a DryRun field,
// independently of the gateway's own check.
func modelsDryRun(t *testing.T, action string) bool {
	t.Helper()
	input, err := ec2Actions[action].parse(map[string]string{"Action": action})
	require.NoError(t, err)
	_, ok := reflect.TypeOf(input).Elem().FieldByName("DryRun")
	return ok
}

// serveEC2 runs a request through EC2_Request and, on error, ErrorHandler, so
// the assertion sees the status and body a client would.
func serveEC2(gw *GatewayConfig, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := setupEC2Request(body, authzAccountID)
	if err := gw.EC2_Request(w, r); err != nil {
		gw.ErrorHandler(w, r, err)
	}
	return w
}

// The gateway has no NATS connection, so any request that reached its handler
// would fail ServerInternal; DryRunOperation proves nothing was dispatched.
func TestEC2Request_DryRunAnswersEveryAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}

	actions := make([]string, 0, len(ec2Actions))
	for action := range ec2Actions {
		actions = append(actions, action)
	}
	slices.Sort(actions)

	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			w := serveEC2(gw, "Action="+action+"&DryRun=true")
			if !modelsDryRun(t, action) {
				assert.NotContains(t, w.Body.String(), awserrors.ErrorDryRunOperation)
				return
			}
			assert.Equal(t, http.StatusPreconditionFailed, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>DryRunOperation</Code>")
			assert.Contains(t, w.Body.String(), "<Message>"+dryRunMessage+"</Message>")
		})
	}
}

func TestEC2Request_DryRunFalseDispatches(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}
	assertPermitted(t, dispatchEC2(t, gw, "Action=CreateVpc&CidrBlock=10.0.0.0/16&DryRun=false"))
}

// A caller the policy refuses is denied rather than told the request would
// have succeeded.
func TestEC2Request_DryRunDeniedCaller(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		statement("Deny", "ec2:CreateVpc", "*"),
	)
	assertDenied(t, dispatchEC2(t, gw, "Action=CreateVpc&CidrBlock=10.0.0.0/16&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, gw, "Action=CreateSecurityGroup&GroupName=g&GroupDescription=d&DryRun=true"))
}

func assertDryRun(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDryRunOperation, err.Error())
}
