package awsapi

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"maps"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/xml/xmlutil"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/internal/testkit"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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
	"ModifyDBSubnetGroup",
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

	"DescribeDBEngineVersions",
	"DescribeOrderableDBInstanceOptions",

	"RegisterDBInstance",
	"SubmitDBStateChange",
	"PollDBCommands",
	"GetDBBootstrapConfig",
	"AcknowledgeDBBootstrap",
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
	_, err := Dispatch(t.Context(), "NotAnRDSAction", nil, nil, testCaller, testEnv)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestDispatch_MalformedScalarIsInvalidParameterValue(t *testing.T) {
	cases := []struct {
		name   string
		action string
		query  map[string]string
	}{
		{name: "Integer", action: "CreateDBInstance", query: map[string]string{"AllocatedStorage": "abc"}},
		{name: "Boolean", action: "CreateDBInstance", query: map[string]string{"MultiAZ": "yes"}},
		{name: "Timestamp", action: "DescribeEvents", query: map[string]string{"StartTime": "not-a-time"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Dispatch(t.Context(), tc.action, tc.query, nil, testCaller, testEnv)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
		})
	}
}

func TestDispatch_OversizedListIsMalformedQueryString(t *testing.T) {
	query := make(map[string]string, 1025)
	for i := 1; i <= 1025; i++ {
		query["Tags.Tag."+strconv.Itoa(i)+".Key"] = "key"
	}

	_, err := Dispatch(t.Context(), "CreateDBInstance", query, nil, testCaller, testEnv)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMalformedQueryString, err.Error())
}

// The customer actions this phase implements forward to the daemon, so they are
// dispatched against a stub responder rather than a nil connection.
var liveActions = []string{
	"CreateDBInstance", "DescribeDBInstances", "ModifyDBInstance",
	"RebootDBInstance", "StartDBInstance", "StopDBInstance", "DeleteDBInstance",
	"DescribeEvents",
	"AddTagsToResource", "RemoveTagsFromResource", "ListTagsForResource",
	"CreateDBSubnetGroup", "DescribeDBSubnetGroups", "ModifyDBSubnetGroup", "DeleteDBSubnetGroup",
	"CreateDBParameterGroup", "DescribeDBParameterGroups", "ModifyDBParameterGroup",
	"DescribeDBParameters", "DeleteDBParameterGroup",
	"CreateDBSnapshot", "DescribeDBSnapshots", "DeleteDBSnapshot",
	"RestoreDBInstanceFromDBSnapshot",
	"DescribeDBInstanceAutomatedBackups",
	"DescribeDBEngineVersions", "DescribeOrderableDBInstanceOptions",
}

// The parameters an action refuses to run without, so a table-driven dispatch
// reaches the handler rather than its own required-parameter check.
var requiredParams = map[string]map[string]string{
	"DescribeOrderableDBInstanceOptions": {"Engine": "postgres"},
}

func dispatchQuery(action string) map[string]string {
	q := map[string]string{"Action": action}
	maps.Copy(q, requiredParams[action])
	return q
}

// One node, so the orderable action's capability probe early-exits on the first
// frame instead of paying the gather's full three-second timeout.
var testEnv = Env{ExpectedNodes: 1}

// What is under test in this file is the action table and the XML envelope, not
// the orchestration behind the subject, so the responder returns a fixed output.
func newStubbedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	respondWith(t, nc, rdsinstance.SubjectCreateDBInstance,
		&rds.CreateDBInstanceOutput{DBInstance: &rds.DBInstance{DBInstanceIdentifier: aws.String("orders-db")}})
	respondWith(t, nc, rdsinstance.SubjectDescribeDBInstances,
		&rds.DescribeDBInstancesOutput{DBInstances: []*rds.DBInstance{}})
	respondWith(t, nc, rdsinstance.SubjectModifyDBInstance, &rds.ModifyDBInstanceOutput{})
	respondWith(t, nc, rdsinstance.SubjectRebootDBInstance, &rds.RebootDBInstanceOutput{})
	respondWith(t, nc, rdsinstance.SubjectStartDBInstance, &rds.StartDBInstanceOutput{})
	respondWith(t, nc, rdsinstance.SubjectStopDBInstance, &rds.StopDBInstanceOutput{})
	respondWith(t, nc, rdsinstance.SubjectDeleteDBInstance, &rds.DeleteDBInstanceOutput{})
	respondWith(t, nc, rdsinstance.SubjectDescribeEvents, &rds.DescribeEventsOutput{})
	respondWith(t, nc, rdsinstance.SubjectAddTagsToResource, &rds.AddTagsToResourceOutput{})
	respondWith(t, nc, rdsinstance.SubjectRemoveTagsFromResource, &rds.RemoveTagsFromResourceOutput{})
	respondWith(t, nc, rdsinstance.SubjectListTagsForResource,
		&rds.ListTagsForResourceOutput{TagList: []*rds.Tag{
			{Key: aws.String("env"), Value: aws.String("prod")},
		}})
	respondWith(t, nc, rdsinstance.SubjectCreateDBSubnetGroup, &rds.CreateDBSubnetGroupOutput{})
	respondWith(t, nc, rdsinstance.SubjectDescribeDBSubnetGroups, &rds.DescribeDBSubnetGroupsOutput{})
	respondWith(t, nc, rdsinstance.SubjectModifyDBSubnetGroup, &rds.ModifyDBSubnetGroupOutput{})
	respondWith(t, nc, rdsinstance.SubjectDeleteDBSubnetGroup, &rds.DeleteDBSubnetGroupOutput{})
	respondWith(t, nc, rdsinstance.SubjectCreateDBParameterGroup, &rds.CreateDBParameterGroupOutput{})
	respondWith(t, nc, rdsinstance.SubjectDescribeDBParameterGroups, &rds.DescribeDBParameterGroupsOutput{})
	respondWith(t, nc, rdsinstance.SubjectModifyDBParameterGroup, &rds.DBParameterGroupNameMessage{})
	respondWith(t, nc, rdsinstance.SubjectDescribeDBParameters, &rds.DescribeDBParametersOutput{})
	respondWith(t, nc, rdsinstance.SubjectDeleteDBParameterGroup, &rds.DeleteDBParameterGroupOutput{})
	respondWith(t, nc, rdsinstance.SubjectCreateDBSnapshot,
		&rds.CreateDBSnapshotOutput{DBSnapshot: &rds.DBSnapshot{DBSnapshotIdentifier: aws.String("orders-db-pre-upgrade")}})
	respondWith(t, nc, rdsinstance.SubjectDescribeDBSnapshots, &rds.DescribeDBSnapshotsOutput{})
	respondWith(t, nc, rdsinstance.SubjectDeleteDBSnapshot, &rds.DeleteDBSnapshotOutput{})
	respondWith(t, nc, rdsinstance.SubjectRestoreDBInstanceFromDBSnapshot,
		&rds.RestoreDBInstanceFromDBSnapshotOutput{DBInstance: &rds.DBInstance{DBInstanceIdentifier: aws.String("orders-db-restored")}})
	respondWith(t, nc, rdsinstance.SubjectDescribeDBInstanceAutomatedBackups,
		&rds.DescribeDBInstanceAutomatedBackupsOutput{})
	// The orderable catalog's capability probe. Unstubbed it would get no
	// ErrNoResponders — Gather publishes and reads an inbox rather than
	// requesting — so every dispatch over the table would pay its full timeout.
	respondWith(t, nc, "ec2.DescribeInstanceTypes", &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: instanceTypeInfo("t3.micro", "t3.small", "t3.medium", "t3.large", "m5.large", "m5.xlarge"),
	})
	return nc
}

func instanceTypeInfo(names ...string) []*ec2.InstanceTypeInfo {
	out := make([]*ec2.InstanceTypeInfo, 0, len(names))
	for _, name := range names {
		out = append(out, &ec2.InstanceTypeInfo{InstanceType: aws.String(name)})
	}
	return out
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
			body, err := Dispatch(t.Context(), action, dispatchQuery(action), nc, testCaller, testEnv)
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
		body, err := Dispatch(t.Context(), action, dispatchQuery(action), nc, testCaller, testEnv)
		require.NoError(t, err, "action %q", action)
		assert.Contains(t, string(body), "<"+action+"Result", "action %q", action)
	}
}

func TestDispatch_OutOfScopeActionIsNotSupported(t *testing.T) {
	for _, action := range outOfScopeActions {
		_, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nil, testCaller, testEnv)
		require.Error(t, err, "action %q", action)
		assert.Equal(t, awserrors.ErrorOperationNotSupported, err.Error(), "action %q", action)
	}
}

func TestDispatch_DescribeDBInstancesReturnsEmptyResultSet(t *testing.T) {
	body, err := Dispatch(t.Context(), "DescribeDBInstances",
		map[string]string{"Action": "DescribeDBInstances", "Version": "2014-10-31"}, newStubbedNATS(t), testCaller, testEnv)
	require.NoError(t, err)

	// The IAM-style envelope the aws-sdk-go query unmarshaler expects, carrying
	// no instances rather than an error.
	assert.Equal(t,
		"<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances></DBInstances>"+
			"</DescribeDBInstancesResult></DescribeDBInstancesResponse>",
		string(body))
}

// AWS nests each tag inside the list rather than flattening it, and the SDK's
// unmarshaler will not find a tag any other way.
func TestDispatch_ListTagsForResourceRendersTheNestedTagList(t *testing.T) {
	body, err := Dispatch(t.Context(), "ListTagsForResource", map[string]string{
		"Action":       "ListTagsForResource",
		"ResourceName": rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", testAccountID, "orders-db"),
	}, newStubbedNATS(t), testCaller, testEnv)
	require.NoError(t, err)

	// Decoded the way a real client decodes it, rather than compared byte-wise:
	// BuildXML emits sibling elements in map order, so <Key> and <Value> swap
	// places between runs.
	var out rds.ListTagsForResourceOutput
	require.NoError(t, xmlutil.UnmarshalXML(&out, xml.NewDecoder(bytes.NewReader(body)), "ListTagsForResourceResult"))

	assert.Equal(t, []*rds.Tag{{Key: aws.String("env"), Value: aws.String("prod")}}, out.TagList)
}

// RDS serializes a tag list under its own locationNameList, so the params
// arrive as Tags.Tag.N.Key. Parsing them as anything else would drop the tags
// and report a success that tagged nothing.
func TestDispatch_AddTagsToResourceParsesTheTagList(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	requests := make(chan rds.AddTagsToResourceInput, 1)
	sub, err := nc.Subscribe(rdsinstance.SubjectAddTagsToResource, func(msg *nats.Msg) {
		var in rds.AddTagsToResourceInput
		if err := json.Unmarshal(msg.Data, &in); err != nil {
			t.Errorf("unmarshal AddTagsToResource request: %v", err)
			return
		}
		requests <- in
		if err := msg.Respond([]byte(`{}`)); err != nil {
			t.Logf("respond on %s: %v", rdsinstance.SubjectAddTagsToResource, err)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	arn := rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", testAccountID, "orders-db")
	_, err = Dispatch(t.Context(), "AddTagsToResource", map[string]string{
		"Action":         "AddTagsToResource",
		"ResourceName":   arn,
		"Tags.Tag.1.Key": "env", "Tags.Tag.1.Value": "prod",
		"Tags.Tag.2.Key": "team", "Tags.Tag.2.Value": "platform",
	}, nc, testCaller, testEnv)
	require.NoError(t, err)

	got := <-requests
	assert.Equal(t, arn, aws.StringValue(got.ResourceName))
	require.Len(t, got.Tags, 2)
	assert.Equal(t, "env", aws.StringValue(got.Tags[0].Key))
	assert.Equal(t, "prod", aws.StringValue(got.Tags[0].Value))
	assert.Equal(t, "team", aws.StringValue(got.Tags[1].Key))
	assert.Equal(t, "platform", aws.StringValue(got.Tags[1].Value))
}

// TagKeys carries no locationNameList, so it keeps the default member wrapper
// even though the sibling tag list does not.
func TestDispatch_RemoveTagsFromResourceParsesMemberTagKeys(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	requests := make(chan rds.RemoveTagsFromResourceInput, 1)
	sub, err := nc.Subscribe(rdsinstance.SubjectRemoveTagsFromResource, func(msg *nats.Msg) {
		var in rds.RemoveTagsFromResourceInput
		if err := json.Unmarshal(msg.Data, &in); err != nil {
			t.Errorf("unmarshal RemoveTagsFromResource request: %v", err)
			return
		}
		requests <- in
		if err := msg.Respond([]byte(`{}`)); err != nil {
			t.Logf("respond on %s: %v", rdsinstance.SubjectRemoveTagsFromResource, err)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = Dispatch(t.Context(), "RemoveTagsFromResource", map[string]string{
		"Action":           "RemoveTagsFromResource",
		"ResourceName":     rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", testAccountID, "orders-db"),
		"TagKeys.member.1": "env",
		"TagKeys.member.2": "team",
	}, nc, testCaller, testEnv)
	require.NoError(t, err)

	assert.Equal(t, []string{"env", "team"}, aws.StringValueSlice((<-requests).TagKeys))
}

// A filtered describe is still an empty result set, not a parse failure: the
// query params have to survive QueryParamsToStruct into the typed input.
func TestDispatch_DescribeDBInstancesWithFilters(t *testing.T) {
	body, err := Dispatch(t.Context(), "DescribeDBInstances", map[string]string{
		"Action":               "DescribeDBInstances",
		"DBInstanceIdentifier": "orders-db",
		"MaxRecords":           "20",
	}, newStubbedNATS(t), testCaller, testEnv)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<DescribeDBInstancesResult")
}
