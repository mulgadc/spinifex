package gateway_rds

import (
	"slices"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gateway/policy"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRegion = "ap-southeast-2"

// The set the gate refuses to customers must be the set the instance role
// grants. Deriving one from the other would let both drift together.
func TestActions_InternalSetMatchesTheInstanceRoleGrant(t *testing.T) {
	var internal []string
	for action, def := range actions {
		if def.internal {
			internal = append(internal, action)
		}
	}
	slices.Sort(internal)
	want := slices.Clone(handlers_rds.InternalAgentActions)
	slices.Sort(want)
	assert.Equal(t, want, internal)
}

func TestAuthorizeCaller_DeniesInternalActionsToCustomers(t *testing.T) {
	callers := []struct {
		name   string
		caller Caller
	}{
		{"customer user", testCaller},
		{"customer role session", Caller{
			AccountID: testAccountID, PrincipalType: principalTypeAssumedRole,
			RoleName: "admin", SessionName: "alice",
		}},
		// The one an rds:* admin grant would otherwise carry all the way in.
		{"customer session named like an instance", Caller{
			AccountID: testAccountID, PrincipalType: principalTypeAssumedRole,
			RoleName: handlers_rds.InstanceRoleName, SessionName: testInstanceID,
		}},
		{"system user rather than a session", Caller{
			AccountID: utils.GlobalAccountID, PrincipalType: "user",
			RoleName: handlers_rds.InstanceRoleName, SessionName: testInstanceID,
		}},
		{"system session under another role", Caller{
			AccountID: utils.GlobalAccountID, PrincipalType: principalTypeAssumedRole,
			RoleName: "ecsInstanceRole", SessionName: testInstanceID,
		}},
	}
	for _, action := range handlers_rds.InternalAgentActions {
		for _, tt := range callers {
			t.Run(action+"/"+tt.name, func(t *testing.T) {
				err := AuthorizeCaller(t.Context(), action, tt.caller)
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
			})
		}
	}
}

// The class gate says the caller is an RDS VM. Which instance it is stays
// authorizeAgent's question, so no NATS lookup happens here.
func TestAuthorizeCaller_AdmitsTheAgentPrincipalClass(t *testing.T) {
	for _, action := range handlers_rds.InternalAgentActions {
		t.Run(action, func(t *testing.T) {
			require.NoError(t, AuthorizeCaller(t.Context(), action, agentCaller()))
		})
	}
}

// Customer actions carry no class requirement: the policy check decides, which
// is what denies the instance role its account's fleet.
func TestAuthorizeCaller_LeavesCustomerActionsToPolicy(t *testing.T) {
	for _, action := range []string{"CreateDBInstance", "DescribeDBInstances", "DeleteDBInstance"} {
		require.NoError(t, AuthorizeCaller(t.Context(), action, testCaller), "action %q", action)
		require.NoError(t, AuthorizeCaller(t.Context(), action, agentCaller()), "action %q", action)
	}
}

func TestAuthorizeCaller_UnknownAction(t *testing.T) {
	err := AuthorizeCaller(t.Context(), "NotAnRDSAction", agentCaller())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestResourceARN_ScopesSingleResourceActions(t *testing.T) {
	tests := []struct {
		action string
		params map[string]string
		want   string
	}{
		{"ModifyDBInstance", map[string]string{"DBInstanceIdentifier": "orders-db"},
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db"},
		{"DeleteDBInstance", map[string]string{"DBInstanceIdentifier": "orders-db"},
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db"},
		{"RebootDBInstance", map[string]string{"DBInstanceIdentifier": "orders-db"},
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db"},
		{"StartDBInstance", map[string]string{"DBInstanceIdentifier": "orders-db"},
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db"},
		{"StopDBInstance", map[string]string{"DBInstanceIdentifier": "orders-db"},
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db"},
		{"DeleteDBSnapshot", map[string]string{"DBSnapshotIdentifier": "orders-db-pre-upgrade"},
			"arn:aws:rds:ap-southeast-2:123456789012:snapshot:orders-db-pre-upgrade"},
		{"DeleteDBSubnetGroup", map[string]string{"DBSubnetGroupName": "db-private"},
			"arn:aws:rds:ap-southeast-2:123456789012:subgrp:db-private"},
		{"ModifyDBParameterGroup", map[string]string{"DBParameterGroupName": "pg16-tuned"},
			"arn:aws:rds:ap-southeast-2:123456789012:pg:pg16-tuned"},
		{"DescribeDBParameters", map[string]string{"DBParameterGroupName": "pg16-tuned"},
			"arn:aws:rds:ap-southeast-2:123456789012:pg:pg16-tuned"},
		{"DeleteDBParameterGroup", map[string]string{"DBParameterGroupName": "pg16-tuned"},
			"arn:aws:rds:ap-southeast-2:123456789012:pg:pg16-tuned"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got, err := ResourceARN(tt.action, testRegion, testAccountID, tt.params)
			require.NoError(t, err)
			assert.Equal(t, []string{tt.want}, got)
		})
	}
}

func TestResourceARN_ScopesSnapshotActionsToSourceAndTarget(t *testing.T) {
	tests := []struct {
		action string
		want   []string
	}{
		{"CreateDBSnapshot", []string{
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db",
			"arn:aws:rds:ap-southeast-2:123456789012:snapshot:orders-db-pre-upgrade",
		}},
		{"RestoreDBInstanceFromDBSnapshot", []string{
			"arn:aws:rds:ap-southeast-2:123456789012:snapshot:orders-db-pre-upgrade",
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db",
		}},
	}
	params := map[string]string{
		"DBInstanceIdentifier": "orders-db", "DBSnapshotIdentifier": "orders-db-pre-upgrade",
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got, err := ResourceARN(tt.action, testRegion, testAccountID, params)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A create names no existing resource, and a plural describe filters rather than
// addresses, so both are evaluated against "*".
func TestResourceARN_UnscopedActions(t *testing.T) {
	unscoped := []string{
		"CreateDBInstance", "DescribeDBInstances", "DescribeDBSnapshots",
		"DescribeDBInstanceAutomatedBackups",
		"CreateDBSubnetGroup", "DescribeDBSubnetGroups",
		"CreateDBParameterGroup", "DescribeDBParameterGroups",
		"DescribeEvents",
		"RegisterDBInstance", "SubmitDBStateChange", "PollDBCommands", "GetDBBootstrapConfig",
	}
	for _, action := range unscoped {
		t.Run(action, func(t *testing.T) {
			// The identifiers a scoped action would consume are supplied on purpose:
			// an unscoped action must ignore them rather than narrow itself.
			got, err := ResourceARN(action, testRegion, testAccountID, map[string]string{
				"DBInstanceIdentifier": "orders-db",
				"DBSnapshotIdentifier": "orders-db-pre-upgrade",
			})
			require.NoError(t, err)
			assert.Equal(t, []string{anyResource}, got)
		})
	}
}

// The escalation the source scope exists to break. Unscoped, these two evaluate
// against "*", and a Deny written on a specific ARN never matches that value —
// so a principal fenced off an instance could snapshot it, restore the copy
// under a name the Deny does not cover, and set a master password of their own.
func TestResourceARN_ResourceScopedDenyReachesTheSnapshotActions(t *testing.T) {
	deniedInstance := handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, testRegion, testAccountID, "prod-db")
	deniedSnapshot := handlers_rds.FormatARN(handlers_rds.ResourceKindDBSnapshot, testRegion, testAccountID, "prod-db-nightly")
	// The common shape: a blanket grant fenced by a resource-scoped deny.
	policies := []iampolicy.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iampolicy.Statement{
			{Effect: iampolicy.EffectAllow, Action: iampolicy.StringOrArr{"rds:*"}, Resource: iampolicy.StringOrArr{"*"}},
			{Effect: iampolicy.EffectDeny, Action: iampolicy.StringOrArr{"rds:*"},
				Resource: iampolicy.StringOrArr{deniedInstance, deniedSnapshot}},
		},
	}}

	tests := []struct {
		name   string
		action string
		params map[string]string
		want   iampolicy.Decision
	}{
		{"snapshot of the fenced instance", "CreateDBSnapshot",
			map[string]string{"DBInstanceIdentifier": "prod-db", "DBSnapshotIdentifier": "prod-db-copy"},
			iampolicy.Deny},
		{"snapshot of another instance", "CreateDBSnapshot",
			map[string]string{"DBInstanceIdentifier": "staging-db", "DBSnapshotIdentifier": "staging-db-copy"},
			iampolicy.Allow},
		{"restore from the fenced snapshot", "RestoreDBInstanceFromDBSnapshot",
			map[string]string{"DBSnapshotIdentifier": "prod-db-nightly", "DBInstanceIdentifier": "prod-db-clone"},
			iampolicy.Deny},
		{"restore from another snapshot", "RestoreDBInstanceFromDBSnapshot",
			map[string]string{"DBSnapshotIdentifier": "staging-db-nightly", "DBInstanceIdentifier": "staging-db-clone"},
			iampolicy.Allow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources, err := ResourceARN(tt.action, testRegion, testAccountID, tt.params)
			require.NoError(t, err)
			got := iampolicy.Allow
			for _, resource := range resources {
				if iampolicy.EvaluateWithKeys(policy.IAMAction("rds", tt.action), resource, policies, nil) == iampolicy.Deny {
					got = iampolicy.Deny
				}
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// The tag actions name their resource by ARN, so the scope validates the ARN it
// was given rather than rebuilding it from an identifier.
func TestResourceARN_TagActionsUseTheSuppliedARN(t *testing.T) {
	arn := handlers_rds.FormatARN(handlers_rds.ResourceKindDBSnapshot, testRegion, testAccountID, "orders-db-pre-upgrade")
	for _, action := range []string{"AddTagsToResource", "RemoveTagsFromResource", "ListTagsForResource"} {
		t.Run(action, func(t *testing.T) {
			got, err := ResourceARN(action, testRegion, testAccountID, map[string]string{"ResourceName": arn})
			require.NoError(t, err)
			assert.Equal(t, []string{arn}, got)
		})
	}
}

func TestResourceARN_RejectsUnusableARNs(t *testing.T) {
	tests := []struct {
		name         string
		resourceName string
	}{
		{"another account", handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, testRegion, "999988887777", "orders-db")},
		{"another region", handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, "us-east-1", testAccountID, "orders-db")},
		{"another service", "arn:aws:ec2:" + testRegion + ":" + testAccountID + ":instance/i-0abc123"},
		{"not an ARN", "orders-db"},
		{"unknown resource type", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":cluster:orders"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResourceARN("ListTagsForResource", testRegion, testAccountID,
				map[string]string{"ResourceName": tt.resourceName})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// A missing identifier is the handler's validation fault to report. Each absent
// member contributes "*" instead of failing ARN resolution.
func TestResourceARN_MissingIdentifierFallsBackToAnyResourcePerMember(t *testing.T) {
	tests := []struct {
		action string
		params map[string]string
		want   []string
	}{
		{"DeleteDBInstance", nil, []string{anyResource}},
		{"DescribeDBParameters", nil, []string{anyResource}},
		{"ListTagsForResource", nil, []string{anyResource}},
		{"CreateDBSnapshot", map[string]string{"DBInstanceIdentifier": "orders-db"}, []string{
			"arn:aws:rds:ap-southeast-2:123456789012:db:orders-db", anyResource,
		}},
		{"RestoreDBInstanceFromDBSnapshot", map[string]string{"DBSnapshotIdentifier": "orders-db-snapshot"}, []string{
			"arn:aws:rds:ap-southeast-2:123456789012:snapshot:orders-db-snapshot", anyResource,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got, err := ResourceARN(tt.action, testRegion, testAccountID, tt.params)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// An unknown action must not resolve to "*": a caller that reached here without
// the dispatcher's action check would otherwise evaluate rds:<garbage> against
// every resource in the account and be told it was fine.
func TestResourceARN_UnknownAction(t *testing.T) {
	_, err := ResourceARN("NotAnRDSAction", testRegion, testAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// Every registered action must resolve a resource without error, so an action
// added with a scope but no identifier in the request cannot fail the request.
func TestResourceARN_EveryActionResolves(t *testing.T) {
	for action := range actions {
		_, err := ResourceARN(action, testRegion, testAccountID, map[string]string{"Action": action})
		require.NoError(t, err, "action %q", action)
	}
}
