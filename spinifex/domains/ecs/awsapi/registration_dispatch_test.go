package awsapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dispatchAccount = "123456789012"

type authCall struct {
	service, action string
	resources       []string
}

func ecsInvoke(t *testing.T, target, body, account string, deny error) ([]authCall, error) {
	t.Helper()
	var calls []authCall
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", target)
	inv := dispatch.Invocation{
		Request:   req,
		AccountID: account,
		Region:    "us-east-1",
		Authorize: func(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
			calls = append(calls, authCall{service, action, resources})
			if action == "PassRole" || service == "iam" {
				return nil
			}
			return deny
		},
	}
	err := ecsawsapi.NewRegistration(ecsawsapi.Deps{}).Dispatch(httptest.NewRecorder(), inv)
	return calls, err
}

func TestRegistration_ResolvesActionFromTarget(t *testing.T) {
	reg := ecsawsapi.NewRegistration(ecsawsapi.Deps{})
	assert.Equal(t, "ecs", reg.Service)
	for target, want := range map[string]string{
		"AmazonEC2ContainerServiceV20141113.RunTask": "RunTask",
		"RunTask": "RunTask",
		"":        "",
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Amz-Target", target)
		assert.Equal(t, want, reg.ResolveAction(req), target)
	}
}

func TestRegistration_InventoryListsRegisteredActions(t *testing.T) {
	inv := ecsawsapi.OperationInventory()
	assert.Contains(t, inv.Registered, "RegisterTaskDefinition")
	assert.NotContains(t, inv.Registered, "NoSuchAction")
}

func TestRegistration_RequestShapeErrors(t *testing.T) {
	tests := []struct {
		name, target, account, body, want string
	}{
		{"missing action", "", dispatchAccount, "{}", awserrors.ErrorMissingAction},
		{"unknown action", "X.NoSuchAction", dispatchAccount, "{}", awserrors.ErrorInvalidAction},
		{"no account", "X.ListClusters", "", "{}", awserrors.ErrorECSServerException},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ecsInvoke(t, tt.target, tt.body, tt.account, nil)
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tt.want, code)
		})
	}
}

// An error that resolves to no awserrors code is a server fault, answered with
// the ServerException every ECS operation declares.
func TestRegistration_UnresolvedErrorIsServerException(t *testing.T) {
	const action = "ListClusters"
	original := ecsawsapi.Actions[action]
	ecsawsapi.Actions[action] = func(context.Context, *nats.Conn, string, []byte, ecsawsapi.PassRoleChecker) (any, error) {
		return nil, errors.New("nats: connection closed")
	}
	t.Cleanup(func() { ecsawsapi.Actions[action] = original })

	_, err := ecsInvoke(t, "X."+action, "{}", dispatchAccount, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorECSServerException, err.Error())
}

func TestRegistration_OversizedBodyIsRefused(t *testing.T) {
	body := strings.Repeat("a", int(sigv4.MaxPayloadLen)+1)
	_, err := ecsInvoke(t, "X.ListClusters", body, dispatchAccount, nil)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, code)
}

func TestRegistration_DenialStopsBeforeTheHandler(t *testing.T) {
	deny := awserrors.Errorf(awserrors.ErrorAccessDenied, "no")
	calls, err := ecsInvoke(t, "X.RegisterTaskDefinition", `{"family":"web","taskRoleArn":"arn:aws:iam::999999999999:role/r"}`, dispatchAccount, deny)

	require.ErrorIs(t, err, deny)
	require.Len(t, calls, 1)
	assert.Equal(t, authCall{service: "ecs", action: "RegisterTaskDefinition"}, authCall{calls[0].service, calls[0].action, nil})
}

// The foreign role is refused after the action is authorized and before the
// request reaches the daemon.
func TestRegistration_ForeignTaskRoleIsRefused(t *testing.T) {
	_, err := ecsInvoke(t, "X.RegisterTaskDefinition", `{"family":"web","taskRoleArn":"arn:aws:iam::999999999999:role/r"}`, dispatchAccount, nil)

	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorECSClientException, code)
	assert.Contains(t, err.Error(), "Role is not valid")
}
