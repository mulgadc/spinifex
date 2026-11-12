package awsapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A service's createdBy names the role behind the session, a task
// definition's registeredBy the session itself; no other action records one.
func TestCreatorARN_RoleForServiceSessionForTaskDefinition(t *testing.T) {
	const role = "arn:aws:iam::123456789012:role/ops/deployer"
	const session = "arn:aws:sts::123456789012:assumed-role/deployer/alice"
	actor := func() (dispatch.Actor, error) {
		return dispatch.Actor{PrincipalType: dispatch.PrincipalTypeAssumedRole, PrincipalARN: role, CallerARN: session}, nil
	}
	assert.Equal(t, role, creatorARN(t.Context(), "CreateService", actor))
	assert.Equal(t, session, creatorARN(t.Context(), "RegisterTaskDefinition", actor))
	assert.Empty(t, creatorARN(t.Context(), "RunTask", actor))
	assert.Empty(t, creatorARN(t.Context(), "CreateService", nil))
	// An unresolved session still names its role, but leaves registeredBy empty.
	noSession := func() (dispatch.Actor, error) { return dispatch.Actor{PrincipalARN: role}, errors.New("no caller ARN") }
	assert.Equal(t, role, creatorARN(t.Context(), "CreateService", noSession))
	assert.Empty(t, creatorARN(t.Context(), "RegisterTaskDefinition", noSession))

	// Dispatch hands the chosen ARN to the daemon in the principal header.
	_, nc, _ := testutil.StartTestJetStream(t)
	got := make(chan string, 1)
	_, err := nc.Subscribe("ecs.RegisterTaskDefinition", func(m *nats.Msg) {
		got <- natsmsg.PrincipalARNFromMsg(m)
		_ = m.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"family":"app"}`))
	req.Header.Set("X-Amz-Target", "X.RegisterTaskDefinition")
	allow := func(string, string, []string, iampolicy.ConditionKeys) error { return nil }
	inv := dispatch.Invocation{Request: req, AccountID: "123456789012", Authorize: allow, Actor: actor}
	require.NoError(t, Deps{NATS: nc}.dispatch(httptest.NewRecorder(), inv))
	assert.Equal(t, session, <-got)
}
