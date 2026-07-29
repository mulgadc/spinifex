package gateway_rds

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

// testCaller is an ordinary customer principal: enough for the customer actions,
// and deliberately not enough for the internal agent actions.
var testCaller = Caller{AccountID: testAccountID, PrincipalType: "user"}

// v1Actions is the RDS v1 namespace as a literal list rather than one derived
// from the table under test, so a dropped or renamed action fails here instead
// of silently redefining the namespace.
var v1Actions = []string{
	"CreateDBInstance",
	"DescribeDBInstances",
	"ModifyDBInstance",
	"DeleteDBInstance",
	"RebootDBInstance",
	"StartDBInstance",
	"StopDBInstance",

	"CreateDBSnapshot",
	"DescribeDBSnapshots",
	"DeleteDBSnapshot",
	"RestoreDBInstanceFromDBSnapshot",

	"DescribeDBInstanceAutomatedBackups",

	"CreateDBSubnetGroup",
	"DescribeDBSubnetGroups",
	"DeleteDBSubnetGroup",

	"CreateDBParameterGroup",
	"DescribeDBParameterGroups",
	"ModifyDBParameterGroup",
	"DescribeDBParameters",
	"DeleteDBParameterGroup",

	"AddTagsToResource",
	"RemoveTagsFromResource",
	"ListTagsForResource",

	"DescribeEvents",

	"RegisterDBInstance",
	"SubmitDBStateChange",
	"PollDBCommands",
	"GetDBBootstrapConfig",
}

// outOfScopeActions are recognised but deliberately not offered in v1.
var outOfScopeActions = []string{
	"CreateDBInstanceReadReplica",
	"PromoteReadReplica",
	"CreateDBCluster",
	"ModifyDBCluster",
	"DeleteDBCluster",
	"DescribeDBClusters",
	"FailoverDBCluster",
	"CreateOptionGroup",
	"ModifyOptionGroup",
	"DeleteOptionGroup",
	"DescribeOptionGroups",
	"RestoreDBInstanceToPointInTime",
}

func TestActions_CoverV1Namespace(t *testing.T) {
	for _, action := range v1Actions {
		assert.True(t, HasAction(action), "action %q should be registered", action)
	}
	for _, action := range outOfScopeActions {
		assert.True(t, HasAction(action), "out-of-scope action %q should be registered so it fails loudly", action)
	}
	assert.Len(t, actions, len(v1Actions)+len(outOfScopeActions),
		"the action table should hold exactly the v1 namespace plus the recognised out-of-scope actions")
}

func TestHasAction_UnknownAction(t *testing.T) {
	for _, action := range []string{"", "RunInstances", "DescribeDBInstance", "createdbinstance"} {
		assert.False(t, HasAction(action), "action %q should not be registered", action)
	}
}

func TestDispatch_UnknownAction(t *testing.T) {
	_, err := Dispatch(t.Context(), "NotAnRDSAction", nil, nil, testCaller)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// The customer actions this phase implements forward to the daemon, so they are
// dispatched against a stub responder rather than a nil connection.
var liveActions = []string{"CreateDBInstance", "DescribeDBInstances"}

// What is under test in this file is the action table and the XML envelope, not
// the orchestration behind the subject, so the responder returns a fixed output.
func newStubbedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	respondWith(t, nc, handlers_rds.SubjectCreateDBInstance,
		&rds.CreateDBInstanceOutput{DBInstance: &rds.DBInstance{DBInstanceIdentifier: aws.String("orders-db")}})
	respondWith(t, nc, handlers_rds.SubjectDescribeDBInstances,
		&rds.DescribeDBInstancesOutput{DBInstances: []*rds.DBInstance{}})
	return nc
}

func respondWith(t *testing.T, nc *nats.Conn, subject string, output any) {
	t.Helper()
	payload, err := json.Marshal(output)
	require.NoError(t, err)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		if err := msg.Respond(payload); err != nil {
			t.Logf("respond on %s: %v", subject, err)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// Every registered action must dispatch to something: either a real response or
// one of the two deliberate rejections. Anything else means an action was wired
// to a handler that fails for an unrelated reason.
func TestDispatch_EveryActionResolves(t *testing.T) {
	nc := newStubbedNATS(t)
	for action := range actions {
		t.Run(action, func(t *testing.T) {
			body, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nc, testCaller)
			if err == nil {
				assert.NotEmpty(t, body, "a successful action must return an XML body")
				return
			}
			// AccessDenied is the fourth legitimate outcome: the internal agent
			// actions gate on principal class, and testCaller is a customer.
			assert.Contains(t,
				[]string{awserrors.ErrorNotImplemented, awserrors.ErrorOperationNotSupported, awserrors.ErrorAccessDenied},
				err.Error(),
				"a stubbed action must reject with NotImplemented, OperationNotSupported or AccessDenied")
		})
	}
}

// The actions this phase implements must have left the pending stub behind.
func TestDispatch_LiveActionsAreNotPending(t *testing.T) {
	nc := newStubbedNATS(t)
	for _, action := range liveActions {
		body, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nc, testCaller)
		require.NoError(t, err, "action %q", action)
		assert.Contains(t, string(body), "<"+action+"Result", "action %q", action)
	}
}

func TestDispatch_PendingActionIsNotImplemented(t *testing.T) {
	_, err := Dispatch(t.Context(), "ModifyDBInstance", map[string]string{"Action": "ModifyDBInstance"}, nil, testCaller)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

func TestDispatch_OutOfScopeActionIsNotSupported(t *testing.T) {
	for _, action := range outOfScopeActions {
		_, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nil, testCaller)
		require.Error(t, err, "action %q", action)
		assert.Equal(t, awserrors.ErrorOperationNotSupported, err.Error(), "action %q", action)
	}
}

func TestDispatch_DescribeDBInstancesReturnsEmptyResultSet(t *testing.T) {
	body, err := Dispatch(t.Context(), "DescribeDBInstances",
		map[string]string{"Action": "DescribeDBInstances", "Version": "2014-10-31"}, newStubbedNATS(t), testCaller)
	require.NoError(t, err)

	// The IAM-style envelope the aws-sdk-go query unmarshaler expects, carrying
	// no instances rather than an error.
	assert.Equal(t,
		"<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances></DBInstances>"+
			"</DescribeDBInstancesResult></DescribeDBInstancesResponse>",
		string(body))
}

// A filtered describe is still an empty result set, not a parse failure: the
// query params have to survive QueryParamsToStruct into the typed input.
func TestDispatch_DescribeDBInstancesWithFilters(t *testing.T) {
	body, err := Dispatch(t.Context(), "DescribeDBInstances", map[string]string{
		"Action":               "DescribeDBInstances",
		"DBInstanceIdentifier": "orders-db",
		"MaxRecords":           "20",
	}, newStubbedNATS(t), testCaller)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<DescribeDBInstancesResult")
}
