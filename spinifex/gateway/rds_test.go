package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rdsTestAccountID = "123456789012"

func setupRDSRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "rds")
	ctx = context.WithValue(ctx, ctxAccountID, rdsTestAccountID)
	return withTestIdentity(req.WithContext(ctx))
}

// A signed customer request: an IAM user with a resolvable identity, which is
// what turns the policy check from the pre-IAM bypass into a real evaluation.
func setupRDSUserRequest(body, accountID string) *http.Request {
	req := setupRDSRequest(body)
	ctx := context.WithValue(req.Context(), ctxAccountID, accountID)
	ctx = context.WithValue(ctx, ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	return req.WithContext(ctx)
}

// An in-guest agent's request: an assumed-role session under the system account
// whose underlying role is the DB VM instance role.
func setupRDSAgentRequest(body, sessionName string) *http.Request {
	req := setupRDSRequest(body)
	roleARN := "arn:aws:iam::" + utils.GlobalAccountID + ":role/" + handlers_rds.InstanceRoleName
	ctx := context.WithValue(req.Context(), ctxAccountID, utils.GlobalAccountID)
	ctx = context.WithValue(ctx, ctxIdentity, sessionName)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeAssumedRole)
	ctx = context.WithValue(ctx, ctxUnderlyingRoleARN, roleARN)
	ctx = context.WithValue(ctx, ctxAssumedRoleARN,
		"arn:aws:sts::"+utils.GlobalAccountID+":assumed-role/"+handlers_rds.InstanceRoleName+"/"+sessionName)
	return req.WithContext(ctx)
}

// Counts policy resolutions, so a test can tell an authorized request from one
// that failed some other guard before the evaluator was ever reached.
type rdsPolicyProbe struct{ consulted int }

// A gateway whose IAM service answers with policies, so the RDS dispatch path
// runs a real evaluation for either principal branch.
func newRDSPolicyGateway(policies []handlers_iam.PolicyDocument) (*GatewayConfig, *rdsPolicyProbe) {
	return newRDSGatewayWithResolver(func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		return policies, nil
	})
}

func newRDSGatewayWithResolver(
	resolve func(accountID, name string) ([]handlers_iam.PolicyDocument, error),
) (*GatewayConfig, *rdsPolicyProbe) {
	probe := &rdsPolicyProbe{}
	counted := func(accountID, name string) ([]handlers_iam.PolicyDocument, error) {
		probe.consulted++
		return resolve(accountID, name)
	}
	// Both branches are wired: an assumed-role principal resolves by role, and a
	// test that only stubbed users would miss it entirely.
	return &GatewayConfig{
		DisableLogging: true,
		Region:         "ap-southeast-2",
		IAMService: &policyMockIAMService{
			getUserPoliciesFn: counted,
			getRolePoliciesFn: counted,
		},
	}, probe
}

func TestRDSRequest_MissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest(""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestRDSRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest("Action=FakeAction"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A malformed percent-encoding is a client error, not an internal one.
func TestRDSRequest_MalformedQueryString(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest("Action=%zz"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMalformedQueryString, err.Error())
}

func TestRDSRequest_MalformedScalarReturnsHTTP400(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	gw := &GatewayConfig{DisableLogging: true, NATSConn: nc, IAMService: allowAllIAMService()}
	for _, body := range []string{
		"Action=CreateDBInstance&AllocatedStorage=abc",
		"Action=CreateDBInstance&MultiAZ=yes",
	} {
		t.Run(body, func(t *testing.T) {
			req := setupRDSRequest(body)
			err := gw.RDS_Request(httptest.NewRecorder(), req)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

			w := httptest.NewRecorder()
			gw.ErrorHandler(w, req, err)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
		})
	}
}

// A known action still needs a NATS connection to reach the control plane, so
// the disconnected case must fail rather than answer from the gateway alone.
func TestRDSRequest_NoNATSConn(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest("Action=DescribeDBInstances"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// The headline of this surface: rds:* on * is the natural admin grant, and it
// must not reach an internal agent action — GetDBBootstrapConfig returns the
// database master password.
func TestRDSRequest_InternalActionsDeniedToCustomerAdmin(t *testing.T) {
	gw, _ := newRDSPolicyGateway(allowPolicy("rds:*"))
	for _, action := range handlers_rds.InternalAgentActions {
		t.Run(action, func(t *testing.T) {
			err := gw.RDS_Request(httptest.NewRecorder(),
				setupRDSUserRequest("Action="+action, rdsTestAccountID))
			require.Error(t, err)
			assertDenied(t, err)
		})
	}
}

// A DB VM's own session passes the class gate and is evaluated against the
// four-action role policy, which admits it: the only thing left to fail on is the
// control plane, absent here.
func TestRDSRequest_InternalActionAdmitsTheAgentPrincipal(t *testing.T) {
	gw, probe := newRDSPolicyGateway(allowPolicy("rds:GetDBBootstrapConfig"))
	err := gw.RDS_Request(httptest.NewRecorder(),
		setupRDSAgentRequest("Action=GetDBBootstrapConfig", "i-0abc123"))
	require.Error(t, err)
	assert.Equal(t, 1, probe.consulted, "the agent's role policy must be evaluated, not bypassed")
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"the agent must be authorized and fail only on the missing NATS connection")
}

// The instance role grants only the internal actions, so the same credentials
// that register an instance cannot describe the account's fleet.
func TestRDSRequest_AgentRoleCannotCallCustomerActions(t *testing.T) {
	gw, _ := newRDSPolicyGateway(allowPolicy("rds:GetDBBootstrapConfig"))
	err := gw.RDS_Request(httptest.NewRecorder(),
		setupRDSAgentRequest("Action=DescribeDBInstances", "i-0abc123"))
	require.Error(t, err)
	assertDenied(t, err)
}

// A policy scoped to db:prod-* has to actually restrict: checked against "*" it
// would read as a restriction and permit the whole account.
func TestRDSRequest_ResourceScopedPolicyRestrictsByIdentifier(t *testing.T) {
	gw, probe := newRDSPolicyGateway(allowPolicyResource("rds:*", "arn:aws:rds:*:*:db:prod-*"))

	err := gw.RDS_Request(httptest.NewRecorder(),
		setupRDSUserRequest("Action=DeleteDBInstance&DBInstanceIdentifier=prod-orders", rdsTestAccountID))
	require.Error(t, err)
	// The probe is what makes the pass meaningful: a guard that failed before the
	// evaluator would leave the same error with no policy consulted at all.
	assert.Equal(t, 1, probe.consulted)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"the in-scope instance must be authorized and fail only on the missing NATS connection")

	err = gw.RDS_Request(httptest.NewRecorder(),
		setupRDSUserRequest("Action=DeleteDBInstance&DBInstanceIdentifier=dev-orders", rdsTestAccountID))
	require.Error(t, err)
	assertDenied(t, err)
}

func TestRDSRequest_SnapshotActionsAuthorizeSourceAndTarget(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		query     string
		resources handlers_iam.StringOrArr
	}{
		{
			name:   "create snapshot",
			action: "rds:CreateDBSnapshot",
			query: "Action=CreateDBSnapshot&DBInstanceIdentifier=orders-db" +
				"&DBSnapshotIdentifier=orders-db-nightly",
			resources: handlers_iam.StringOrArr{
				handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db"),
				handlers_rds.FormatARN(handlers_rds.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly"),
			},
		},
		{
			name:   "restore snapshot",
			action: "rds:RestoreDBInstanceFromDBSnapshot",
			query: "Action=RestoreDBInstanceFromDBSnapshot&DBSnapshotIdentifier=orders-db-nightly" +
				"&DBInstanceIdentifier=orders-db-restored",
			resources: handlers_iam.StringOrArr{
				handlers_rds.FormatARN(handlers_rds.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly"),
				handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db-restored"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policies := []handlers_iam.PolicyDocument{{
				Version: "2012-10-17",
				Statement: []handlers_iam.Statement{{
					Effect: "Allow", Action: handlers_iam.StringOrArr{tt.action}, Resource: tt.resources,
				}},
			}}
			gw, probe := newRDSPolicyGateway(policies)
			err := gw.RDS_Request(httptest.NewRecorder(), setupRDSUserRequest(tt.query, rdsTestAccountID))
			require.Error(t, err)
			assert.Equal(t, 1, probe.consulted)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
		})
	}
}

func TestRDSRequest_SnapshotActionsUseOnePolicySnapshot(t *testing.T) {
	source := handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db")
	target := handlers_rds.FormatARN(handlers_rds.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly")
	resolution := 0
	gw, probe := newRDSGatewayWithResolver(func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		resolution++
		if resolution == 1 {
			return allowPolicyResource("rds:CreateDBSnapshot", source), nil
		}
		return allowPolicyResource("rds:CreateDBSnapshot", target), nil
	})

	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSUserRequest(
		"Action=CreateDBSnapshot&DBInstanceIdentifier=orders-db&DBSnapshotIdentifier=orders-db-nightly",
		rdsTestAccountID,
	))
	require.Error(t, err)
	assertDenied(t, err)
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
			target: handlers_rds.FormatARN(handlers_rds.ResourceKindDBSnapshot, "ap-southeast-2", rdsTestAccountID, "orders-db-nightly"),
		},
		{
			name: "restore snapshot", action: "rds:RestoreDBInstanceFromDBSnapshot",
			query: "Action=RestoreDBInstanceFromDBSnapshot&DBSnapshotIdentifier=orders-db-nightly" +
				"&DBInstanceIdentifier=orders-db-restored",
			target: handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, "ap-southeast-2", rdsTestAccountID, "orders-db-restored"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policies := []handlers_iam.PolicyDocument{{
				Version: "2012-10-17",
				Statement: []handlers_iam.Statement{
					{Effect: "Allow", Action: handlers_iam.StringOrArr{tt.action}, Resource: handlers_iam.StringOrArr{"*"}},
					{Effect: "Deny", Action: handlers_iam.StringOrArr{tt.action}, Resource: handlers_iam.StringOrArr{tt.target}},
				},
			}}
			gw, probe := newRDSPolicyGateway(policies)
			err := gw.RDS_Request(httptest.NewRecorder(), setupRDSUserRequest(tt.query, rdsTestAccountID))
			require.Error(t, err)
			assert.Equal(t, 1, probe.consulted)
			assertDenied(t, err)
		})
	}
}

// Creates and plural describes name no one resource, so a policy scoped to a
// single instance must not be read as permitting them.
func TestRDSRequest_UnscopedActionsEvaluateAgainstAnyResource(t *testing.T) {
	gw, _ := newRDSPolicyGateway(allowPolicyResource("rds:*", "arn:aws:rds:*:*:db:prod-orders"))
	err := gw.RDS_Request(httptest.NewRecorder(),
		setupRDSUserRequest("Action=CreateDBInstance&DBInstanceIdentifier=prod-orders", rdsTestAccountID))
	require.Error(t, err)
	assertDenied(t, err)
}

// Cross-account references are refused at the ARN, not merely unauthorized:
// snapshot sharing is out of v1, and an evaluator that sees a foreign account is
// one policy away from allowing it.
func TestRDSRequest_CrossAccountARNIsRejectedNotEvaluated(t *testing.T) {
	gw, _ := newRDSGatewayWithResolver(func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
		t.Error("a cross-account ARN must never reach policy evaluation")
		return nil, nil
	})
	foreign := handlers_rds.FormatARN(handlers_rds.ResourceKindDBInstance, "ap-southeast-2", "999988887777", "orders-db")
	err := gw.RDS_Request(httptest.NewRecorder(),
		setupRDSUserRequest("Action=ListTagsForResource&ResourceName="+foreign, rdsTestAccountID))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

// A request with no resolvable identity is denied outright: there is no
// principal to evaluate, so the gate refuses before consulting any policy.
func TestRDSRequest_NoIdentityDenied(t *testing.T) {
	gw, probe := newRDSPolicyGateway(nil)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=DescribeDBInstances"))
	ctx := context.WithValue(req.Context(), ctxService, "rds")
	ctx = context.WithValue(ctx, ctxAccountID, rdsTestAccountID)

	err := gw.RDS_Request(httptest.NewRecorder(), req.WithContext(ctx))
	require.Error(t, err)
	assertDenied(t, err)
	assert.Zero(t, probe.consulted, "a request with no identity must not be evaluated at all")
}

// RDS errors must use the IAM-style <ErrorResponse> envelope: the aws-sdk-go
// query unmarshaler rejects the EC2 <Response><Errors> shape for this service.
func TestRDSErrorHandler_UsesIAMEnvelope(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBInstance"), errNotImplementedForTest)

	body := w.Body.String()
	assert.Contains(t, body, "<ErrorResponse>")
	assert.Contains(t, body, "<Code>"+awserrors.ErrorNotImplemented+"</Code>")
}

func TestRDSErrorHandler_UsesRDSUnsupportedActionWording(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBCluster"),
		errors.New(awserrors.ErrorOperationNotSupported))

	body := w.Body.String()
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, body, "RDS")
	assert.Contains(t, body, "v1")
	assert.NotContains(t, body, "registry")
}

var errNotImplementedForTest = errors.New(awserrors.ErrorNotImplemented)
