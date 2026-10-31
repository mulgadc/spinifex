package awsapi_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rdsTestAccountID = gwtest.AccountID

// Counts policy resolutions, so a test can tell an authorized request from one
// that failed some other guard before the evaluator was ever reached.
type rdsPolicyProbe struct{ consulted int }

// serveRDS dispatches body to a registration with no NATS connection, as the
// pipeline would for the harness's caller.
func serveRDS(h *gwtest.Harness, body string) error {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return h.Dispatch(rdsawsapi.NewRegistration(rdsawsapi.Deps{}), rdsawsapi.ServiceName, httptest.NewRecorder(), req)
}

// A harness whose IAM service answers with policies, so the RDS dispatch path
// runs a real evaluation for either principal branch.
func newRDSPolicyHarness(policies []iamdomain.PolicyDocument) (*gwtest.Harness, *rdsPolicyProbe) {
	return newRDSHarnessWithResolver(func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return policies, nil
	})
}

func newRDSHarnessWithResolver(
	resolve func(accountID, name string) ([]iamdomain.PolicyDocument, error),
) (*gwtest.Harness, *rdsPolicyProbe) {
	probe := &rdsPolicyProbe{}
	// Both branches are wired: an assumed-role principal resolves by role, and a
	// test that only stubbed users would miss it entirely.
	return gwtest.FromResolver(func(accountID, name string) ([]iamdomain.PolicyDocument, error) {
		probe.consulted++
		return resolve(accountID, name)
	}), probe
}

// An in-guest agent's request: an assumed-role session under the system account
// whose underlying role is the DB VM instance role.
func asRDSAgent(h *gwtest.Harness, sessionName string) {
	roleARN := "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + rdsinstance.InstanceRoleName
	assumed := "arn:aws:sts::" + awsidentifiers.GlobalAccountID + ":assumed-role/" + rdsinstance.InstanceRoleName + "/" + sessionName
	h.AccountID = awsidentifiers.GlobalAccountID
	h.Caller = &credential.Principal{
		AccountID:         awsidentifiers.GlobalAccountID,
		Identity:          sessionName,
		PrincipalType:     "assumed-role",
		UnderlyingRoleARN: roleARN,
		AssumedRoleARN:    assumed,
		CallerARN:         assumed,
	}
}

func allowRDSPolicy(action string) []iamdomain.PolicyDocument {
	return allowRDSPolicyResource(action, "*")
}

func allowRDSPolicyResource(action, resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{{
			Effect:   "Allow",
			Action:   iamdomain.StringOrArr{action},
			Resource: iamdomain.StringOrArr{resource},
		}},
	}}
}

func TestRDSRequest_MissingAction(t *testing.T) {
	err := serveRDS(gwtest.AllowAll(), "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestRDSRequest_UnknownAction(t *testing.T) {
	err := serveRDS(gwtest.AllowAll(), "Action=FakeAction")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A malformed percent-encoding is a client error, not an internal one.
func TestRDSRequest_MalformedQueryString(t *testing.T) {
	err := serveRDS(gwtest.AllowAll(), "Action=%zz")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMalformedQueryString, err.Error())
}

// A known action still needs a NATS connection to reach the control plane, so
// the disconnected case must fail rather than answer from the gateway alone.
func TestRDSRequest_NoNATSConn(t *testing.T) {
	err := serveRDS(gwtest.AllowAll(), "Action=DescribeDBInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// The headline of this surface: rds:* on * is the natural admin grant, and it
// must not reach an internal agent action — GetDBBootstrapConfig returns the
// database master password.
func TestRDSRequest_InternalActionsDeniedToCustomerAdmin(t *testing.T) {
	h, _ := newRDSPolicyHarness(allowRDSPolicy("rds:*"))
	for _, action := range rdsinstance.InternalAgentActions {
		t.Run(action, func(t *testing.T) {
			err := serveRDS(h, "Action="+action)
			require.Error(t, err)
			gwtest.AssertDenied(t, err)
		})
	}
}

// A DB VM's own session passes the class gate and is evaluated against the
// four-action role policy, which admits it: the only thing left to fail on is the
// control plane, absent here.
func TestRDSRequest_InternalActionAdmitsTheAgentPrincipal(t *testing.T) {
	h, probe := newRDSPolicyHarness(allowRDSPolicy("rds:GetDBBootstrapConfig"))
	asRDSAgent(h, "i-0abc123")
	err := serveRDS(h, "Action=GetDBBootstrapConfig")
	require.Error(t, err)
	assert.Equal(t, 1, probe.consulted, "the agent's role policy must be evaluated, not bypassed")
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"the agent must be authorized and fail only on the missing NATS connection")
}

// The instance role grants only the internal actions, so the same credentials
// that register an instance cannot describe the account's fleet.
func TestRDSRequest_AgentRoleCannotCallCustomerActions(t *testing.T) {
	h, _ := newRDSPolicyHarness(allowRDSPolicy("rds:GetDBBootstrapConfig"))
	asRDSAgent(h, "i-0abc123")
	err := serveRDS(h, "Action=DescribeDBInstances")
	require.Error(t, err)
	gwtest.AssertDenied(t, err)
}

// A policy scoped to db:prod-* has to actually restrict: checked against "*" it
// would read as a restriction and permit the whole account.
func TestRDSRequest_ResourceScopedPolicyRestrictsByIdentifier(t *testing.T) {
	h, probe := newRDSPolicyHarness(allowRDSPolicyResource("rds:*", "arn:aws:rds:*:*:db:prod-*"))

	err := serveRDS(h, "Action=DeleteDBInstance&DBInstanceIdentifier=prod-orders")
	require.Error(t, err)
	// The probe is what makes the pass meaningful: a guard that failed before the
	// evaluator would leave the same error with no policy consulted at all.
	assert.Equal(t, 1, probe.consulted)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"the in-scope instance must be authorized and fail only on the missing NATS connection")

	err = serveRDS(h, "Action=DeleteDBInstance&DBInstanceIdentifier=dev-orders")
	require.Error(t, err)
	gwtest.AssertDenied(t, err)
}

func TestRDSRequest_SnapshotActionsAuthorizeSourceAndTarget(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		query     string
		resources iamdomain.StringOrArr
	}{
		{
			name:   "create snapshot",
			action: "rds:CreateDBSnapshot",
			query: "Action=CreateDBSnapshot&DBInstanceIdentifier=orders-db" +
				"&DBSnapshotIdentifier=orders-db-nightly",
			resources: iamdomain.StringOrArr{
				rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db"),
				rdsinstance.FormatARN(rdsinstance.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly"),
			},
		},
		{
			name:   "restore snapshot",
			action: "rds:RestoreDBInstanceFromDBSnapshot",
			query: "Action=RestoreDBInstanceFromDBSnapshot&DBSnapshotIdentifier=orders-db-nightly" +
				"&DBInstanceIdentifier=orders-db-restored",
			resources: iamdomain.StringOrArr{
				rdsinstance.FormatARN(rdsinstance.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly"),
				rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db-restored"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policies := []iamdomain.PolicyDocument{{
				Version: "2012-10-17",
				Statement: []iamdomain.Statement{{
					Effect: "Allow", Action: iamdomain.StringOrArr{tt.action}, Resource: tt.resources,
				}},
			}}
			h, probe := newRDSPolicyHarness(policies)
			err := serveRDS(h, tt.query)
			require.Error(t, err)
			assert.Equal(t, 1, probe.consulted)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
		})
	}
}

func TestRDSRequest_SnapshotActionsUseOnePolicySnapshot(t *testing.T) {
	source := rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db")
	target := rdsinstance.FormatARN(rdsinstance.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly")
	resolution := 0
	h, probe := newRDSHarnessWithResolver(func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		resolution++
		if resolution == 1 {
			return allowRDSPolicyResource("rds:CreateDBSnapshot", source), nil
		}
		return allowRDSPolicyResource("rds:CreateDBSnapshot", target), nil
	})

	err := serveRDS(h, "Action=CreateDBSnapshot&DBInstanceIdentifier=orders-db&DBSnapshotIdentifier=orders-db-nightly")
	require.Error(t, err)
	gwtest.AssertDenied(t, err)
	assert.Equal(t, 1, probe.consulted)
}

func TestRDSRequest_SnapshotActionsHonorTargetDeny(t *testing.T) {
	tests := []struct {
		name   string
		action string
		query  string
		target string
	}{
		{
			name: "create snapshot", action: "rds:CreateDBSnapshot",
			query: "Action=CreateDBSnapshot&DBInstanceIdentifier=orders-db" +
				"&DBSnapshotIdentifier=orders-db-nightly",
			target: rdsinstance.FormatARN(rdsinstance.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly"),
		},
		{
			name: "restore snapshot", action: "rds:RestoreDBInstanceFromDBSnapshot",
			query: "Action=RestoreDBInstanceFromDBSnapshot&DBSnapshotIdentifier=orders-db-nightly" +
				"&DBInstanceIdentifier=orders-db-restored",
			target: rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db-restored"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policies := []iamdomain.PolicyDocument{{
				Version: "2012-10-17",
				Statement: []iamdomain.Statement{
					{Effect: "Allow", Action: iamdomain.StringOrArr{tt.action}, Resource: iamdomain.StringOrArr{"*"}},
					{Effect: "Deny", Action: iamdomain.StringOrArr{tt.action}, Resource: iamdomain.StringOrArr{tt.target}},
				},
			}}
			h, probe := newRDSPolicyHarness(policies)
			err := serveRDS(h, tt.query)
			require.Error(t, err)
			assert.Equal(t, 1, probe.consulted)
			gwtest.AssertDenied(t, err)
		})
	}
}

// Creates and plural describes name no one resource, so a policy scoped to a
// single instance must not be read as permitting them.
func TestRDSRequest_UnscopedActionsEvaluateAgainstAnyResource(t *testing.T) {
	h, _ := newRDSPolicyHarness(allowRDSPolicyResource("rds:*", "arn:aws:rds:*:*:db:prod-orders"))
	err := serveRDS(h, "Action=CreateDBInstance&DBInstanceIdentifier=prod-orders")
	require.Error(t, err)
	gwtest.AssertDenied(t, err)
}

// Cross-account references are refused at the ARN, not merely unauthorized:
// snapshot sharing is out of v1, and an evaluator that sees a foreign account is
// one policy away from allowing it.
func TestRDSRequest_CrossAccountARNIsRejectedNotEvaluated(t *testing.T) {
	h, _ := newRDSHarnessWithResolver(func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		t.Error("a cross-account ARN must never reach policy evaluation")
		return nil, nil
	})
	foreign := rdsinstance.FormatARN(rdsinstance.ResourceKindDBInstance, "ap-southeast-2", "999988887777", "orders-db")
	err := serveRDS(h, "Action=ListTagsForResource&ResourceName="+foreign)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

// A request with no resolvable identity is denied outright: there is no
// principal to evaluate, so the gate refuses before consulting any policy.
func TestRDSRequest_NoIdentityDenied(t *testing.T) {
	h, probe := newRDSPolicyHarness(nil)
	h.Caller = &credential.Principal{AccountID: rdsTestAccountID}

	err := serveRDS(h, "Action=DescribeDBInstances")
	require.Error(t, err)
	gwtest.AssertDenied(t, err)
	assert.Zero(t, probe.consulted, "a request with no identity must not be evaluated at all")
}

// RDS carries its scopes on the dispatch table itself, so the two cannot
// disagree the way the other services' can. What can still go wrong is an action
// landing with no scopes and nobody noticing it now evaluates against "*", so
// the gate is the account-level half: every entry here is a written decision
// that the action addresses no single resource.
var rdsAccountLevelActions = map[string]string{
	// Creates: the resource does not exist yet, which is how AWS evaluates them.
	"CreateDBInstance":       "the instance does not exist until this call makes it",
	"CreateDBSubnetGroup":    "the subnet group does not exist until this call makes it",
	"CreateDBParameterGroup": "the parameter group does not exist until this call makes it",

	// Describes that filter rather than address one resource.
	"DescribeDBInstances":                "filters the account's instances",
	"DescribeDBSnapshots":                "filters the account's snapshots",
	"DescribeDBInstanceAutomatedBackups": "filters the account's automated backups",
	"DescribeDBSubnetGroups":             "filters the account's subnet groups",
	"DescribeDBParameterGroups":          "filters the account's parameter groups",
	"DescribeEvents":                     "the event ring is per-account and a filter names no one resource",

	// Catalogs read no per-account state at all.
	"DescribeDBEngineVersions":           "a static engine catalog, not tenant state",
	"DescribeOrderableDBInstanceOptions": "cluster capability, not tenant state",

	// Internal agent actions. AuthorizeCaller refuses these to every customer
	// principal by class before any policy is evaluated, so a resource scope
	// would narrow nothing.
	"RegisterDBInstance":     "agent-only, gated by principal class",
	"SubmitDBStateChange":    "agent-only, gated by principal class",
	"PollDBCommands":         "agent-only, gated by principal class",
	"GetDBBootstrapConfig":   "agent-only, gated by principal class",
	"AcknowledgeDBBootstrap": "agent-only, gated by principal class",
}

// TestRDSScopeTableIsExhaustive is what stops the next RDS action being added
// with a silent account-wide grant. Every served action is either scoped or
// carries a written reason for being account-level, and an action in both sets
// or in neither fails.
func TestRDSScopeTableIsExhaustive(t *testing.T) {
	unsupported := rdsawsapi.UnsupportedActionNames()

	for _, action := range rdsawsapi.ActionNames() {
		if slices.Contains(unsupported, action) {
			continue
		}
		_, accountLevel := rdsAccountLevelActions[action]
		assert.NotEqual(t, rdsawsapi.HasScope(action), accountLevel,
			"rds action %q must either carry a scope in domains/rds/awsapi/handler.go or a written reason "+
				"in rdsAccountLevelActions, not both and not neither", action)
	}

	for action := range rdsAccountLevelActions {
		assert.True(t, rdsawsapi.HasAction(action),
			"rdsAccountLevelActions names %q, which RDS does not serve: remove it", action)
		assert.NotContains(t, unsupported, action,
			"rdsAccountLevelActions names %q, which is recognised but not offered: remove it", action)
	}
}
