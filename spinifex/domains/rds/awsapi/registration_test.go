package awsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func invocation(body, accountID string, actor dispatch.Actor, actorErr error, authorize dispatch.AuthorizeFunc) dispatch.Invocation {
	if authorize == nil {
		authorize = func(string, string, []string, iampolicy.ConditionKeys) error { return nil }
	}
	return dispatch.Invocation{
		Request:   httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)),
		AccountID: accountID,
		Region:    "ap-southeast-2",
		Authorize: authorize,
		Actor:     func() (dispatch.Actor, error) { return actor, actorErr },
	}
}

func TestCallerFromActor_NameFollowsPrincipalType(t *testing.T) {
	roleARN := "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + rdsinstance.InstanceRoleName
	session := dispatch.Actor{
		AccountID: awsidentifiers.GlobalAccountID, PrincipalType: principalTypeAssumedRole,
		PrincipalARN: roleARN, SessionName: "i-abc", UserName: "ignored",
	}
	got := CallerFromActor(session)
	assert.Equal(t, "i-abc", got.SessionName)
	assert.Equal(t, rdsinstance.InstanceRoleName, got.RoleName)

	user := dispatch.Actor{AccountID: "000000000002", PrincipalType: "user", UserName: "alice", SessionName: "ignored"}
	got = CallerFromActor(user)
	assert.Equal(t, "alice", got.SessionName)
	assert.Empty(t, got.RoleName, "a user has no role name")
}

func TestCallerFromActor_RoleNameNeverFromSessionName(t *testing.T) {
	// The caller picks its own session name at AssumeRole time; naming it after
	// the agent role must not make it the agent role.
	got := CallerFromActor(dispatch.Actor{
		AccountID: "000000000002", PrincipalType: principalTypeAssumedRole,
		PrincipalARN: "arn:aws:iam::000000000002:role/tenant", SessionName: rdsinstance.InstanceRoleName,
	})
	assert.Equal(t, "tenant", got.RoleName)
	assert.Error(t, requireAgentPrincipal(context.Background(), got))
}

func TestDispatch_RequestShapeErrors(t *testing.T) {
	actor := dispatch.Actor{AccountID: "000000000002", PrincipalType: "user", UserName: "alice"}
	tests := []struct {
		name, body, account, code string
	}{
		{"malformed query", "Action=%zz", "000000000002", awserrors.ErrorMalformedQueryString},
		{"missing action", "", "000000000002", awserrors.ErrorMissingAction},
		{"unknown action", "Action=Nope", "000000000002", awserrors.ErrorInvalidAction},
		{"no account", "Action=DescribeDBInstances", "", awserrors.ErrorServerInternal},
		{"no NATS", "Action=DescribeDBInstances", "000000000002", awserrors.ErrorServerInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Deps{}.dispatch(httptest.NewRecorder(), invocation(tt.body, tt.account, actor, nil, nil))
			code, _, _ := awserrors.ResolveErrorDetail(err)
			assert.Equal(t, tt.code, code)
		})
	}
}

func TestDispatch_InternalActionRefusedBeforePolicy(t *testing.T) {
	consulted := false
	authorize := func(string, string, []string, iampolicy.ConditionKeys) error { consulted = true; return nil }
	actor := dispatch.Actor{AccountID: "000000000002", PrincipalType: "user", UserName: "alice"}
	err := Deps{}.dispatch(httptest.NewRecorder(),
		invocation("Action=GetDBBootstrapConfig", "000000000002", actor, nil, authorize))
	code, _, _ := awserrors.ResolveErrorDetail(err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
	assert.False(t, consulted, "no customer grant is evaluated against an internal action")
}

func TestDispatch_PolicyDenialStopsBeforeHandler(t *testing.T) {
	deny := func(string, string, []string, iampolicy.ConditionKeys) error {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	// An actor error leaves the actor empty; the policy check still decides.
	err := Deps{}.dispatch(httptest.NewRecorder(),
		invocation("Action=DescribeDBInstances", "000000000002", dispatch.Actor{}, errors.New("no arn"), deny))
	code, _, _ := awserrors.ResolveErrorDetail(err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
}

func TestOperationInventory_SplitsUnsupported(t *testing.T) {
	inv := OperationInventory()
	assert.Contains(t, inv.Registered, "CreateDBInstance")
	assert.Contains(t, inv.Unsupported, "CreateDBCluster")
	assert.NotContains(t, inv.Unsupported, "CreateDBInstance")
	require.NoError(t, dispatch.NewBuilder().Register(NewRegistration(Deps{})))
}
