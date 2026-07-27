package gateway_rds

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

// v1Actions is the RDS v1 namespace from rds-v1.md §1. Keeping it as a literal
// list rather than deriving it from the table under test means a dropped or
// renamed action fails here instead of silently redefining the namespace.
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
	_, err := Dispatch(t.Context(), "NotAnRDSAction", nil, nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// Every registered action must dispatch to something: either a real response or
// one of the two deliberate rejections. Anything else means an action was wired
// to a handler that fails for an unrelated reason.
func TestDispatch_EveryActionResolves(t *testing.T) {
	for action := range actions {
		t.Run(action, func(t *testing.T) {
			body, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nil, testAccountID)
			if err == nil {
				assert.NotEmpty(t, body, "a successful action must return an XML body")
				return
			}
			assert.Contains(t,
				[]string{awserrors.ErrorNotImplemented, awserrors.ErrorOperationNotSupported},
				err.Error(),
				"a stubbed action must reject with NotImplemented or OperationNotSupported")
		})
	}
}

func TestDispatch_PendingActionIsNotImplemented(t *testing.T) {
	_, err := Dispatch(t.Context(), "CreateDBInstance", map[string]string{"Action": "CreateDBInstance"}, nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

func TestDispatch_OutOfScopeActionIsNotSupported(t *testing.T) {
	for _, action := range outOfScopeActions {
		_, err := Dispatch(t.Context(), action, map[string]string{"Action": action}, nil, testAccountID)
		require.Error(t, err, "action %q", action)
		assert.Equal(t, awserrors.ErrorOperationNotSupported, err.Error(), "action %q", action)
	}
}

func TestDispatch_DescribeDBInstancesReturnsEmptyResultSet(t *testing.T) {
	body, err := Dispatch(t.Context(), "DescribeDBInstances",
		map[string]string{"Action": "DescribeDBInstances", "Version": "2014-10-31"}, nil, testAccountID)
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
	}, nil, testAccountID)
	require.NoError(t, err)
	assert.Contains(t, string(body), "<DescribeDBInstancesResult")
}
