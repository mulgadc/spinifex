package gateway_rds

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDBID       = "orders-db"
	testInstanceID = "i-0abc123"
)

// agentCaller is the identity an in-guest agent presents: an assumed-role
// session under the system account whose session name is its own instance ID.
func agentCaller() Caller {
	return Caller{
		AccountID:     utils.GlobalAccountID,
		PrincipalType: principalTypeAssumedRole,
		RoleName:      handlers_rds.InstanceRoleName,
		SessionName:   testInstanceID,
	}
}

// newIndexedNATS starts JetStream with one instance already in the reverse
// index, which is the state the launch path leaves behind.
func newIndexedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := handlers_rds.NewService(nc, "ap-southeast-2")
	require.NoError(t, svc.PutInstanceIndex(context.Background(), testInstanceID, handlers_rds.InstanceIndexEntry{
		AccountID:            testAccountID,
		DBInstanceIdentifier: testDBID,
		VMGeneration:         1,
	}))
	return nc
}

func TestAuthorizeAgent_ResolvesInstanceFromIndex(t *testing.T) {
	nc := newIndexedNATS(t)
	id, err := authorizeAgent(t.Context(), nc, agentCaller(), "")
	require.NoError(t, err)
	assert.Equal(t, testAccountID, id.AccountID, "the account comes from the index, not the caller")
	assert.Equal(t, testDBID, id.DBInstanceIdentifier)
	assert.Equal(t, testInstanceID, id.InstanceID)
}

// The agent's own credentials are minted under the system account, so the
// customer account it acts for can only come from the index.
func TestAuthorizeAgent_IgnoresRequestedIdentifierWhenItMatches(t *testing.T) {
	nc := newIndexedNATS(t)
	id, err := authorizeAgent(t.Context(), nc, agentCaller(), testDBID)
	require.NoError(t, err)
	assert.Equal(t, testDBID, id.DBInstanceIdentifier)
}

func TestAuthorizeAgent_RejectsNonAgentCallers(t *testing.T) {
	nc := newIndexedNATS(t)
	tests := []struct {
		name   string
		caller Caller
	}{
		{"plain IAM user", Caller{AccountID: utils.GlobalAccountID, PrincipalType: "user", RoleName: handlers_rds.InstanceRoleName, SessionName: testInstanceID}},
		{"root", Caller{AccountID: utils.GlobalAccountID, PrincipalType: "root", RoleName: handlers_rds.InstanceRoleName, SessionName: testInstanceID}},
		{"customer account", Caller{AccountID: testAccountID, PrincipalType: principalTypeAssumedRole, RoleName: handlers_rds.InstanceRoleName, SessionName: testInstanceID}},
		{"another role", Caller{AccountID: utils.GlobalAccountID, PrincipalType: principalTypeAssumedRole, RoleName: "ecsInstanceRole", SessionName: testInstanceID}},
		{"unresolvable role", Caller{AccountID: utils.GlobalAccountID, PrincipalType: principalTypeAssumedRole, RoleName: "", SessionName: testInstanceID}},
		{"no session name", Caller{AccountID: utils.GlobalAccountID, PrincipalType: principalTypeAssumedRole, RoleName: handlers_rds.InstanceRoleName, SessionName: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := authorizeAgent(t.Context(), nc, tt.caller, "")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
		})
	}
}

// A session name that is not in the index is not an RDS VM, whatever role it
// holds. This is the case a forged RoleSessionName lands in.
func TestAuthorizeAgent_RejectsUnindexedInstance(t *testing.T) {
	nc := newIndexedNATS(t)
	caller := agentCaller()
	caller.SessionName = "i-0notadatabase"

	_, err := authorizeAgent(t.Context(), nc, caller, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// One agent must not be able to act on another instance by naming it.
func TestAuthorizeAgent_RejectsCrossInstanceIdentifier(t *testing.T) {
	nc := newIndexedNATS(t)
	_, err := authorizeAgent(t.Context(), nc, agentCaller(), "someone-elses-db")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// Before any DB instance exists the system bucket is absent. That is a denial,
// not an internal error, so a probe cannot distinguish the two.
func TestAuthorizeAgent_RejectsWhenNoInstancesExist(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := authorizeAgent(t.Context(), nc, agentCaller(), "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// Every internal action runs the gate before it touches the control plane, so a
// customer caller is refused without a NATS round trip.
func TestInternalActions_GateBeforeDispatch(t *testing.T) {
	for _, action := range []string{"RegisterDBInstance", "SubmitDBStateChange", "PollDBCommands", "GetDBBootstrapConfig"} {
		t.Run(action, func(t *testing.T) {
			_, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nil, testCaller)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
		})
	}
}

// An empty poll is the steady state: the agent gets no command and no error.
func TestPollDBCommands_ReturnsEmptyOnTimeout(t *testing.T) {
	nc := newIndexedNATS(t)
	out, err := PollDBCommands(t.Context(),
		&PollDBCommandsInput{WaitTimeSeconds: 1}, nc, agentCaller())
	require.NoError(t, err)
	assert.Empty(t, out.(*PollDBCommandsOutput).Commands)
}

func TestPollDBCommands_DeliversPublishedCommand(t *testing.T) {
	nc := newIndexedNATS(t)
	subject := handlers_rds.BusCommandSubject(testAccountID, testDBID)

	type result struct {
		out any
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := PollDBCommands(context.Background(), &PollDBCommandsInput{WaitTimeSeconds: 5}, nc, agentCaller())
		done <- result{out, err}
	}()

	data, err := json.Marshal(handlers_rds.Command{CommandID: "cmd-1", Type: "set-password"})
	require.NoError(t, err)

	// The command channel is a live subscription, so a directive published
	// before the poll subscribes is gone by contract. Republishing until the
	// poll returns synchronises without reaching into the subscription.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			require.NoError(t, r.err)
			cmds := r.out.(*PollDBCommandsOutput).Commands
			require.Len(t, cmds, 1)
			assert.Equal(t, "cmd-1", cmds[0].CommandID)
			assert.Equal(t, "set-password", cmds[0].Type)
			return
		case <-ticker.C:
			require.NoError(t, nc.Publish(subject, data))
		case <-time.After(10 * time.Second):
			t.Fatal("poll did not return the published command")
		}
	}
}

// The agent's replies ride the next poll and are republished for the issuer,
// which correlates them by command ID.
func TestPollDBCommands_RepublishesReplies(t *testing.T) {
	nc := newIndexedNATS(t)
	replySub, err := nc.SubscribeSync(handlers_rds.BusCommandReplySubject(testAccountID, testDBID))
	require.NoError(t, err)
	defer func() { _ = replySub.Unsubscribe() }()

	_, err = PollDBCommands(t.Context(), &PollDBCommandsInput{
		WaitTimeSeconds: 1,
		Replies: []handlers_rds.CommandReply{
			{CommandID: "cmd-1", Status: handlers_rds.CommandStatusSucceeded},
			{CommandID: "", Status: handlers_rds.CommandStatusFailed}, // dropped: no correlation
		},
	}, nc, agentCaller())
	require.NoError(t, err)

	msg, err := replySub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var reply handlers_rds.CommandReply
	require.NoError(t, json.Unmarshal(msg.Data, &reply))
	assert.Equal(t, "cmd-1", reply.CommandID)
	assert.Equal(t, handlers_rds.CommandStatusSucceeded, reply.Status)

	_, err = replySub.NextMsg(100 * time.Millisecond)
	assert.Error(t, err, "an uncorrelatable reply must not be republished")
}

// The XML marshaller omits a nil pointer entirely but renders an empty value
// field as <Tag></Tag>. MasterUserPassword is a pointer for exactly that reason:
// an attach response must not carry the element at all.
func TestBootstrapConfigXML_OmitsPasswordOnAttach(t *testing.T) {
	password := "s3cr3t-master-pw"
	base := handlers_rds.GetDBBootstrapConfigOutput{
		DBInstanceIdentifier: testDBID,
		Engine:               "postgres",
		MasterUsername:       "postgres",
		Port:                 5432,
		Parameters:           []handlers_rds.Parameter{{Name: "shared_buffers", Value: "128MB"}},
	}

	initialize := base
	initialize.Mode = handlers_rds.BootstrapModeInitialize
	initialize.MasterUserPassword = &password
	body := marshalResult(t, "GetDBBootstrapConfig", &initialize)
	assert.Contains(t, body, "<MasterUserPassword>"+password+"</MasterUserPassword>")

	attach := base
	attach.Mode = handlers_rds.BootstrapModeAttach
	body = marshalResult(t, "GetDBBootstrapConfig", &attach)
	assert.NotContains(t, body, "MasterUserPassword", "the element must be absent, not empty")
	assert.NotContains(t, body, password)

	// The rest of the payload still has to reach the agent on an attach. The
	// marshaller emits a struct's children in map order, so each element is
	// asserted on its own rather than as an ordered pair.
	assert.Contains(t, body, "<Port>5432</Port>")
	assert.Contains(t, body, "<member>")
	assert.Contains(t, body, "<Name>shared_buffers</Name>")
	assert.Contains(t, body, "<Value>128MB</Value>")
}

func marshalResult(t *testing.T, action string, out any) string {
	t.Helper()
	body, err := utils.MarshalToXML(utils.GenerateIAMXMLPayload(action, out))
	require.NoError(t, err)
	return string(body)
}

func TestPollWait_ClampsToWindow(t *testing.T) {
	tests := []struct {
		name      string
		requested int64
		want      time.Duration
	}{
		{"unset uses the default", 0, defaultPollWait},
		{"negative uses the default", -5, defaultPollWait},
		{"below the floor is raised", 1, minPollWait},
		{"in range is honoured", 10, 10 * time.Second},
		{"above the ceiling is capped", 600, maxPollWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pollWait(tt.requested))
		})
	}
}
